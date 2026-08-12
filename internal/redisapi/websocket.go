package redisapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSClient provides WebSocket-based access to the AceTeam Redis API.
// Used for real-time pub/sub operations instead of HTTP polling.
type WSClient struct {
	baseURL   string
	token     string
	conn      *websocket.Conn
	connMu    sync.RWMutex
	connected bool

	// connEstablishedAt is when the current conn was published (connectLocked).
	// Guarded by connMu, like conn/connected. handleDisconnect reads it against
	// pongWait to decide whether the connection PROVED itself before it died --
	// see the #746 note on reconnectBackoff below. Zero when conn is nil.
	connEstablishedAt time.Time

	// writeMu serializes gorilla "write methods" (WriteMessage,
	// SetWriteDeadline) on conn. gorilla supports exactly ONE concurrent
	// writer and panics with "concurrent write to websocket connection"
	// otherwise, which kills the whole worker process (#720).
	//
	// It is deliberately NOT connMu: connMu guards the conn POINTER, is taken
	// shared (RLock) by every reader, and is released before any write
	// happens, so it serializes nothing on the wire. Keeping them separate
	// also keeps pointer reads cheap while a slow write is in flight.
	//
	// Lock order: never acquire connMu while holding writeMu. sendMessage
	// releases connMu before taking writeMu; Close takes connMu then writeMu.
	writeMu sync.Mutex

	// writeTimeout bounds a single WriteMessage. Serializing writes means a
	// blocked write no longer stalls just its own caller, it stalls every
	// publisher queued behind writeMu, so the write has to be bounded.
	// Overridden in tests; see defaultWriteTimeout for the value rationale.
	writeTimeout time.Duration

	// pingInterval and pongWait drive the keepalive (#734). pongWait is the
	// read deadline: nothing arriving within it means the socket is dead.
	// pingInterval is how often we send a ping to provoke traffic. Both are
	// overridden in tests; see defaultPongWait for the value rationale.
	pingInterval time.Duration
	pongWait     time.Duration

	// Message handlers
	handlers   map[string]func(WSMessage)
	handlersMu sync.RWMutex

	// Subscriptions to restore on reconnect
	subscriptions   map[string]bool
	subscriptionsMu sync.RWMutex

	// Control channels
	done     chan struct{}
	stopOnce sync.Once

	// loopsOnce guards the background goroutines. readLoop and pingLoop are
	// deliberately long-lived: they re-read c.conn every iteration, so they
	// survive a reconnect and must NOT be started per connection. Connect is
	// callable again whenever connected is false (the #723 retry loop does
	// exactly that), and without this a second Connect would start a second
	// reader -- gorilla permits exactly one concurrent reader (#734, #740).
	loopsOnce sync.Once

	// Reconnection settings.
	//
	// reconnectBackoff is guarded by connMu (#728). It is advanced by
	// nextReconnectDelay, which takes connMu for the whole read-modify-write
	// and releases it before the caller sleeps. Nothing may touch it unlocked:
	// reconnect used to, and a torn or stale read there shortens a backoff.
	//
	// The keepalive (#734) is what makes that guard load-bearing rather than
	// theoretical: handleDisconnect used to fire almost never, and now fires on
	// every read-deadline expiry, so the reconnect goroutine advances this
	// field routinely instead of exotically.
	//
	// It is reset to initialReconnectBackoff in exactly two places, neither of
	// which is "a dial succeeded" (that was #746's bug -- see the note there):
	//   - handleDisconnect, when the connection being torn down PROVED itself
	//     (survived long enough that connEstablishedAt shows real traffic kept
	//     its read deadline alive, not just that the dial worked).
	//   - reconnect, after honoring a server-requested retry_after on a 429
	//     (#747): once the server has spoken, it is the authority, so the local
	//     schedule resumes from the start rather than from wherever doubling
	//     had already taken it.
	//
	// reconnectEnabled and maxBackoff are set once in NewWSClient and never
	// written again, so they need no guard.
	reconnectEnabled bool
	reconnectBackoff time.Duration
	maxBackoff       time.Duration

	// jitterFrac draws the randomness nextReconnectDelay adds on top of the
	// schedule. It is a field rather than a package var so a test can make the
	// delay deterministic without mutating shared state that a previous test's
	// still-running reconnect goroutine might be reading. Set once in
	// NewWSClient, before any goroutine exists. Must return [0,1).
	jitterFrac func() float64

	// Reconnect callbacks fired after a successful reconnection
	reconnectCallbacks   []func()
	reconnectCallbacksMu sync.RWMutex

	// Debug callback
	debugFunc func(format string, args ...any)
}

// WSClientConfig holds configuration for the WebSocket client.
type WSClientConfig struct {
	// BaseURL is the AceTeam API base URL (e.g., "https://aceteam.ai")
	BaseURL string

	// Token is the device_api_token from device authentication
	Token string

	// ReconnectEnabled enables automatic reconnection (default: true)
	ReconnectEnabled bool

	// DebugFunc is an optional callback for debug logging
	DebugFunc func(format string, args ...any)
}

// WSMessage represents a message received from or sent to the WebSocket.
type WSMessage struct {
	Type     string         `json:"type"`
	Channel  string         `json:"channel,omitempty"`
	Channels []string       `json:"channels,omitempty"`
	Message  map[string]any `json:"message,omitempty"`
	Error    string         `json:"error,omitempty"`

	// Consume-related fields (job delivery protocol)
	Queue     string            `json:"queue,omitempty"`
	Queues    []string          `json:"queues,omitempty"`
	Group     string            `json:"group,omitempty"`
	Consumer  string            `json:"consumer,omitempty"`
	Count     int               `json:"count,omitempty"`
	BlockMs   int               `json:"blockMs,omitempty"`
	ID        string            `json:"id,omitempty"`        // Stream message ID (job delivery)
	MessageID string            `json:"messageId,omitempty"` // Ack message ID
	Data      map[string]string `json:"data,omitempty"`      // Job data fields from stream
}

// initialReconnectBackoff is the delay before the first reconnect attempt, and
// the value the schedule returns to once a connection has PROVED itself (see
// handleDisconnect, #746) or once a 429's retry_after has been honored in full
// (see reconnect, #747). It lives in one place so the constructor and both
// resets cannot drift.
const initialReconnectBackoff = time.Second

// provenConnDurationMultiplier is how many pongWait cycles a connection must
// survive before handleDisconnect treats it as having proved itself and resets
// reconnectBackoff (#746).
//
// It is expressed as a multiple of pongWait, not a fixed duration, because
// pongWait is what times out a silent connection: surviving several of those
// cycles is only possible if the read deadline was genuinely pushed out by
// real traffic (a pong or a message; see armKeepalive), not merely by a dial
// that happened to succeed. A single cycle is not enough margin -- a
// connection that dies right at (rather than comfortably past) the deadline
// should not count -- so this is "a few", matching the guidance in #746 itself.
const provenConnDurationMultiplier = 3

// Reconnect jitter.
//
// The schedule used to be exactly 1, 2, 4, ... 60 seconds on every node, with no
// randomness anywhere in this file. Whenever the control plane blips, every node
// in the fleet is disconnected at nearly the same instant and then redials in
// lockstep, against an endpoint that answers 429 (typed by rateLimitFromUpgrade
// above). That is a self-inflicted thundering herd, and the keepalive in #734
// makes it worse rather than better: handleDisconnect now fires on every
// read-deadline expiry instead of almost never, so this path runs routinely.
//
// The scheme is ADDITIVE and UPWARD-ONLY: the sleep is drawn uniformly from
// [base, 2*base), where base is the deterministic step. The two textbook
// alternatives were both rejected for the same reason, which is the only
// property this package cannot trade away:
//
//   - Full jitter, rand(0, base), has a floor of zero. A node that has just been
//     told 429 could redial milliseconds later, and the mean delay is HALF the
//     current schedule, so fleet-wide retry pressure doubles. That is the shape
//     of #443, where tight retries burned the node's daily Redis-API quota and
//     locked it out for about a day, and it is the direction #728 calls out as
//     the one that matters. cmd/work.go's rate-limit path already refuses to
//     "poll tighter than retry_after" for the same reason.
//   - Decorrelated jitter, min(cap, rand(base, prev*3)), can also return to base
//     repeatedly, so a long outage can sit near one second indefinitely. It also
//     wants the previous SLEEP as its state, which would collide with
//     reconnectBackoff holding the deterministic schedule that connectLocked
//     resets on a successful dial.
//
// Additive jitter is never shorter than what shipped before, and its spread is
// the full width of the current step, which is exactly enough to decorrelate a
// fleet whose nodes all hold the same step after a shared blip.
//
// The jittered sleep is deliberately NOT clamped to maxBackoff. maxBackoff caps
// the SCHEDULE, not the sleep. Clamping the result would collapse the spread to
// zero at the ceiling, which is precisely the worst case: a long control-plane
// outage puts every node at the cap, and they would all dial together again.
// The cost is a worst-case sleep of two minutes rather than one, reached only
// after roughly seven consecutive failed dials, and publishes fall back to HTTP
// throughout (see Client.Publish).
//
// Note what this does NOT fix on its own, so nobody reads more into the jitter
// than is there: the flap in #734's own "what this makes worse" (a peer that is
// alive but never pongs, torn down every pongWait, reconnecting at one second,
// forever) has a period dominated by pongWait, not by the backoff. One second of
// jitter per 46-second cycle random-walks those phases apart far too slowly to
// matter. Two things actually address it, both implemented alongside this
// jitter rather than by it: #746, not resetting the schedule for a connection
// that never proved itself (see the connEstablishedAt note on reconnectBackoff
// above, and handleDisconnect), and #747, honoring the typed rate-limit hint
// that connectLocked already builds and reconnect used to discard (see
// reconnect). #747 outranks this jitter for a 429ing endpoint, because it uses
// the interval the server actually sent rather than one we invented.

// NewWSClient creates a new WebSocket client.
func NewWSClient(cfg WSClientConfig) *WSClient {
	reconnectEnabled := cfg.ReconnectEnabled
	// Default to true if not explicitly set
	if cfg.BaseURL != "" && !cfg.ReconnectEnabled {
		reconnectEnabled = true
	}

	return &WSClient{
		baseURL:          cfg.BaseURL,
		token:            cfg.Token,
		handlers:         make(map[string]func(WSMessage)),
		subscriptions:    make(map[string]bool),
		done:             make(chan struct{}),
		reconnectEnabled: reconnectEnabled,
		reconnectBackoff: initialReconnectBackoff,
		maxBackoff:       time.Minute,
		writeTimeout:     defaultWriteTimeout,
		pingInterval:     defaultPingInterval,
		pongWait:         defaultPongWait,
		jitterFrac:       rand.Float64,
		debugFunc:        cfg.DebugFunc,
	}
}

// debug logs a message if debug function is configured
func (c *WSClient) debug(format string, args ...any) {
	if c.debugFunc != nil {
		c.debugFunc(format, args...)
	}
}

// Connect establishes the WebSocket connection.
func (c *WSClient) Connect(ctx context.Context) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.connected {
		return nil
	}

	if err := c.connectLocked(ctx); err != nil {
		return err
	}

	// Both loops outlive any single connection; see loopsOnce.
	c.loopsOnce.Do(func() {
		go c.readLoop()
		go c.pingLoop()
	})

	return nil
}

// connectLocked establishes connection (caller must hold connMu lock)
func (c *WSClient) connectLocked(ctx context.Context) error {
	// Convert HTTP URL to WebSocket URL
	wsURL, err := c.getWSURL()
	if err != nil {
		return fmt.Errorf("failed to build WebSocket URL: %w", err)
	}

	c.debug("ws: connecting to %s", wsURL)

	// Set up headers with auth token
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.token)

	// Connect with context
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if resp != nil {
			// A rejected upgrade carries the same rate-limit hint the REST
			// routes do. Return it TYPED so callers that retry (issue #723) can
			// honor retry_after instead of falling into a generic 2s..2min
			// backoff and polling tighter than the server asked -- the tight
			// loop that burned the daily quota in #443.
			if resp.StatusCode == http.StatusTooManyRequests {
				return fmt.Errorf("WebSocket connection failed: %w", rateLimitFromUpgrade(resp))
			}
			return fmt.Errorf("WebSocket connection failed with status %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("WebSocket connection failed: %w", err)
	}

	// Arm the keepalive BEFORE publishing the connection (#734).
	//
	// SetReadDeadline, SetPongHandler and SetPingHandler are gorilla READ
	// methods, so they must not run concurrently with ReadMessage. Doing this
	// while conn is still private -- readLoop cannot see it until c.conn is
	// assigned under connMu -- is what makes that safe. The handlers only ever
	// fire from inside ReadMessage afterwards, i.e. on the read goroutine.
	c.armKeepalive(conn)

	c.conn = conn
	c.connected = true
	// Record when THIS connection was established, not whether it will reset
	// the backoff (#746). A dial succeeding is not evidence the connection is
	// useful -- a peer that is alive but never pongs dials fine and then dies
	// at the next pongWait every time -- so the reset itself happens later, in
	// handleDisconnect, only if this connection survives long enough to prove
	// it. The caller holds connMu for write, which is what makes this
	// assignment safe against nextReconnectDelay running in a reconnect
	// goroutine (#728).
	c.connEstablishedAt = time.Now()

	c.debug("ws: connected successfully")

	// Restore subscriptions if any
	c.subscriptionsMu.RLock()
	channels := make([]string, 0, len(c.subscriptions))
	for ch := range c.subscriptions {
		channels = append(channels, ch)
	}
	c.subscriptionsMu.RUnlock()

	if len(channels) > 0 {
		c.debug("ws: restoring %d subscriptions", len(channels))
		// Send subscribe without holding locks
		go func() {
			if err := c.Subscribe(context.Background(), channels...); err != nil {
				c.debug("ws: failed to restore subscriptions: %v", err)
			}
		}()
	}

	return nil
}

// getWSURL converts the HTTP base URL to a WebSocket URL
func (c *WSClient) getWSURL() (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}

	// Convert scheme
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}

	// Add WebSocket path
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/fabric/redis/ws"

	return u.String(), nil
}

// readLoop continuously reads messages from the WebSocket
func (c *WSClient) readLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.connMu.RLock()
		conn := c.conn
		connected := c.connected
		c.connMu.RUnlock()

		if !connected || conn == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				// The read deadline expired: nothing at all arrived for
				// pongWait, not even a pong for our pings. Before #734 this
				// case did not exist and the loop simply blocked forever, so a
				// half-open socket stayed "connected" while nothing flowed.
				c.debug("ws: no traffic for %v, treating connection as dead", c.pongWait)
			} else {
				c.debug("ws: read error: %v", err)
			}
			c.handleDisconnect(conn)
			continue
		}

		// Any inbound frame is evidence the peer is alive, so extend the
		// deadline on data too, not just on pongs. That matters if a proxy or
		// server ever stops honoring ping frames: a busy connection then still
		// stays up on its own traffic.
		_ = conn.SetReadDeadline(time.Now().Add(c.pongWait))

		var msg WSMessage
		if err := json.Unmarshal(message, &msg); err != nil {
			c.debug("ws: failed to parse message: %v", err)
			continue
		}

		c.debug("ws: received message type=%s", msg.Type)

		// Handle message based on type
		c.handlersMu.RLock()
		handler, ok := c.handlers[msg.Type]
		c.handlersMu.RUnlock()

		if ok {
			handler(msg)
		}

		// Also call wildcard handler if set
		c.handlersMu.RLock()
		wildcardHandler, ok := c.handlers["*"]
		c.handlersMu.RUnlock()

		if ok {
			wildcardHandler(msg)
		}
	}
}

// armKeepalive installs the read deadline and the control-frame handlers on a
// freshly dialed connection. Caller must not have published conn yet: these are
// gorilla read methods and cannot race ReadMessage.
func (c *WSClient) armKeepalive(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(c.pongWait))

	conn.SetPongHandler(func(string) error {
		// Our ping came back, so the round trip is intact. Runs on the read
		// goroutine, from inside ReadMessage.
		return conn.SetReadDeadline(time.Now().Add(c.pongWait))
	})

	conn.SetPingHandler(func(appData string) error {
		// A server-side keepalive counts as liveness too. We still have to
		// answer it, which gorilla's default handler would have done for us,
		// so replicate that here including its one-second bound and its error
		// swallowing: a peer that has already gone away must not turn into a
		// read error from the pong.
		//
		// The short bound is the point. This runs on the read goroutine, and
		// WriteControl waits on gorilla's write semaphore, which an in-flight
		// WriteMessage can hold for defaultWriteTimeout. A longer deadline here
		// would park the ONLY reader behind write congestion, processing no
		// inbound messages while it waited. Skipping a pong is cheaper: the
		// timeout is swallowed below, we keep reading, and if the peer really
		// does give up on us the read deadline notices within pongWait.
		_ = conn.SetReadDeadline(time.Now().Add(c.pongWait))
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(pongReplyWait))
		if err == websocket.ErrCloseSent {
			return nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil
		}
		return err
	})
}

// pingLoop sends a ping every pingInterval so an idle connection still proves
// itself. It is long-lived and re-reads c.conn each tick, so it spans
// reconnects; see loopsOnce.
//
// The ping is written with WriteControl, which does NOT need writeMu. Verified
// against the vendored gorilla source rather than taken on faith: WriteControl
// takes gorilla's own single-writer semaphore (c.mu) for the duration of the
// frame, reads writeErr under writeErrMu, and sets the deadline directly on the
// underlying net.Conn (goroutine-safe) instead of touching the c.writeDeadline
// field that writeMu exists to guard. The package docs say the same thing in
// one line: "The Close and WriteControl methods can be called concurrently with
// all other methods."
func (c *WSClient) pingLoop() {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}

		c.connMu.RLock()
		conn := c.conn
		connected := c.connected
		c.connMu.RUnlock()

		if !connected || conn == nil {
			continue
		}

		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingWriteWait)); err != nil {
			// Deliberately advisory: the read deadline is the single authority
			// on liveness. A ping can fail simply because a large WriteMessage
			// is holding gorilla's write semaphore, and tearing down a healthy
			// connection because a control frame queued too long would be a
			// self-inflicted outage. A genuinely fatal failure is not lost:
			// gorilla latches it in writeErr, so the next publish tears the
			// connection down, and no pongs will arrive either way, so the
			// read deadline fires within pongWait regardless.
			c.debug("ws: ping failed: %v", err)
		}
	}
}

// handleDisconnect tears down conn and schedules a reconnect.
//
// conn is the connection the caller observed failing. If the client has already
// moved on to a different connection the call is a no-op, so a slow failure
// cannot close a healthy socket that reconnect established in the meantime.
func (c *WSClient) handleDisconnect(conn *websocket.Conn) {
	c.connMu.Lock()
	if conn != nil && c.conn != conn {
		// Already replaced; whoever swapped it owns the old connection.
		c.connMu.Unlock()
		return
	}
	c.connected = false
	if c.conn != nil {
		// #746: reset the backoff schedule here, on teardown, and only for a
		// connection that PROVED itself -- never unconditionally on dial
		// success (that was the bug: connectLocked used to reset on every
		// successful dial regardless of what happened after). A peer that is
		// alive but never pongs (a proxy stripping control frames, a server
		// that stopped ponging) then gets torn down every pongWait, the redial
		// succeeds, the schedule resets to 1s, and the node redials roughly
		// every pongWait+1s FOREVER instead of backing off toward maxBackoff
		// like any other repeated failure.
		//
		// "Proved itself" is measured as this connection's age against
		// provenConnDurationMultiplier*pongWait; see the comment there for why
		// age is a sound proxy for real traffic having kept it alive.
		if !c.connEstablishedAt.IsZero() {
			proven := time.Since(c.connEstablishedAt) >= time.Duration(provenConnDurationMultiplier)*c.pongWait
			if proven {
				c.reconnectBackoff = initialReconnectBackoff
			}
		}
		c.conn.Close()
		c.conn = nil
		c.connEstablishedAt = time.Time{}
	}
	c.connMu.Unlock()

	if !c.reconnectEnabled {
		return
	}

	// Attempt reconnection with backoff
	go c.reconnect()
}

// nextReconnectDelay returns how long the caller should wait before its next
// reconnect attempt, and advances the schedule for the attempt after that.
//
// The read, the doubling and the clamp all happen in ONE critical section under
// connMu, and the caller sleeps on the returned value instead of re-reading the
// field. reconnect used to do all of it with no lock at all, racing the resets
// in handleDisconnect and connectLocked, all of which run under connMu (#728).
// It also read the field twice, once to log the delay and once to sleep it, so
// a reset landing between the two made the node retry sooner than it had just
// announced.
//
// Both defects point the same way, and it is the way that hurts: anything that
// silently shortens a backoff moves toward the #443 restart storm referenced
// above connectWithBackoff in cmd/work.go, where tight retries burned the node's
// daily Redis-API quota and locked it out for about a day.
//
// connMu is reused deliberately rather than adding a third mutex to this file.
// Both resets (handleDisconnect for #746, reconnect for #747) already run under
// connMu for other reasons, so connMu costs no extra lock and no nested acquire
// there either. connMu is NOT held across the sleep.
//
// The returned delay is JITTERED; the stored schedule is not. See the jitter
// note above initialReconnectBackoff for why it is additive and upward-only,
// and why the returned sleep is deliberately allowed to exceed maxBackoff.
func (c *WSClient) nextReconnectDelay() time.Duration {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	base := c.reconnectBackoff

	c.reconnectBackoff *= 2
	if c.reconnectBackoff > c.maxBackoff {
		c.reconnectBackoff = c.maxBackoff
	}

	return c.jitteredLocked(base)
}

// jitteredLocked spreads base over [base, 2*base). Caller holds connMu.
//
// Only the RETURNED sleep is randomized. c.reconnectBackoff keeps holding the
// clean doubling, so the schedule stays predictable, a reset (handleDisconnect
// or reconnect; see the reconnectBackoff field comment) still means exactly
// "back to one second", and nothing accumulates randomness across attempts.
func (c *WSClient) jitteredLocked(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	frac := rand.Float64
	if c.jitterFrac != nil {
		frac = c.jitterFrac
	}
	return base + time.Duration(frac()*float64(base))
}

// sleepUnlessDone waits for d, returning false if Close happened first (#741).
//
// The bare time.Sleep this replaces was not cancellable and, worse, reconnect
// did not re-check c.done afterwards, so a Close landing during a backoff still
// let the loop dial and then mark a closed client connected: IsConnected()
// reported true after Close returned, and the socket it opened was left dangling
// until the next iteration noticed done and returned without closing it.
//
// The window is the whole remaining sleep, so it was widest exactly when the
// control plane was unhealthy and the schedule had grown to a minute. The
// keepalive (#734) is what turns this from rare into ordinary: reconnect used to
// run almost never and now runs on every read-deadline expiry. Jitter can push
// the sleep to two minutes, which widens the same window again.
//
// Not addressed here, and deliberately: connectLocked is still called with
// context.Background(), so the dial itself is bound only by the 10s handshake
// timeout rather than by the caller's lifetime. Giving the client a real context
// is a larger change than this loop.
func (c *WSClient) sleepUnlessDone(d time.Duration) bool {
	if d <= 0 {
		// Still honour a Close that has already happened.
		select {
		case <-c.done:
			return false
		default:
			return true
		}
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-c.done:
		return false
	case <-t.C:
		return true
	}
}

// reconnectRateLimitChunk bounds a single sleep against a server-requested
// retry_after (#747), mirroring cmd/work.go's connectRateLimitChunk: honoring a
// very long wait (e.g. retry_after: 86400 on a daily quota) must not turn into
// an hours-long timer that only re-checks Close once at the very end.
const reconnectRateLimitChunk = 90 * time.Second

// sleepUnlessDoneUntil is sleepUnlessDone's chunked form, for a wait whose
// total length may be very large (a server-requested retry_after, #747). It
// sleeps in bounded chunks, re-checking c.done between them -- the same policy
// cmd/work.go's sleepUntilCtx uses for the HTTP retry path (connectWithBackoffLabeled),
// so the WebSocket path honors retry_after the same way the REST path already
// does rather than inventing a second scheme. Returns false the moment Close
// happens, at any point in the wait.
func (c *WSClient) sleepUnlessDoneUntil(deadline time.Time) bool {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			select {
			case <-c.done:
				return false
			default:
				return true
			}
		}
		chunk := remaining
		if chunk > reconnectRateLimitChunk {
			chunk = reconnectRateLimitChunk
		}
		if !c.sleepUnlessDone(chunk) {
			return false
		}
	}
}

// fireReconnectCallbacks runs the registered OnReconnect callbacks.
//
// Split out of reconnect because #748 needs it from two call sites: the normal
// "reconnect dialed successfully" path, and the "a racing Connect() already
// re-established the connection" path (see bailIfAlreadyReconnected). Callbacks
// run without any lock held -- they may call sendMessage, which takes connMu
// for read, so holding connMu here would deadlock a callback against its own
// caller.
func (c *WSClient) fireReconnectCallbacks() {
	c.reconnectCallbacksMu.RLock()
	cbs := make([]func(), len(c.reconnectCallbacks))
	copy(cbs, c.reconnectCallbacks)
	c.reconnectCallbacksMu.RUnlock()

	for _, cb := range cbs {
		cb()
	}
}

// bailIfAlreadyReconnected reports whether a racing Connect() has already
// re-established the connection (#748), and if so fires the reconnect
// callbacks before telling the caller to stop.
//
// Connect() itself deliberately never fires OnReconnect (see its doc comment:
// only actual reconnects do, not the initial connect). But from a caller's
// point of view, THIS is a reconnect -- the socket had gone down and
// handleDisconnect had already spawned this goroutine to bring it back -- it
// just happened to be Connect() that won the race, most likely the #723
// background retry loop (enableWebSocketWithRetry) polling in on its own
// schedule. If reconnect silently returned here without firing anything, a
// caller that relies on OnReconnect to re-arm (e.g. re-subscribing at a higher
// layer) would end up connected but with nothing flowing -- the exact failure
// #734 exists to prevent. So reconnect, not Connect, is the one piece of code
// responsible for firing the callbacks in this race, and it does so exactly
// once, for whichever side's connection is the one left standing.
func (c *WSClient) bailIfAlreadyReconnected() bool {
	c.connMu.RLock()
	connected := c.connected
	c.connMu.RUnlock()
	if !connected {
		return false
	}
	c.debug("ws: reconnect: already connected (a racing Connect() won), firing callbacks instead of dialing again")
	c.fireReconnectCallbacks()
	return true
}

// reconnect attempts to reconnect with exponential backoff.
func (c *WSClient) reconnect() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		if c.bailIfAlreadyReconnected() {
			return
		}

		delay := c.nextReconnectDelay()
		c.debug("ws: attempting reconnect in %v", delay)
		if !c.sleepUnlessDone(delay) {
			return
		}

		// Re-check under the SAME lock that performs the dial (#748). c.done
		// is re-checked by sleepUnlessDone above, but c.connected was not
		// re-checked at all: a Connect() landing anywhere during the sleep
		// (bailIfAlreadyReconnected only catches one landing BEFORE the sleep
		// started) would set c.conn = A, connected = true, and then this
		// goroutine would wake up and call connectLocked unconditionally,
		// which dials a SECOND connection B, overwrites c.conn with it, and
		// never closes A. A is orphaned: an open FD and a server-side
		// connection that nothing will ever read (readLoop follows c.conn,
		// which now points at B) or close.
		c.connMu.Lock()
		if c.connected {
			c.connMu.Unlock()
			c.debug("ws: reconnect: Connect() already re-established the connection during the backoff sleep, skipping duplicate dial")
			c.fireReconnectCallbacks()
			return
		}
		err := c.connectLocked(context.Background())
		c.connMu.Unlock()

		if err != nil {
			c.debug("ws: reconnect failed: %v", err)

			// #747: a rejected dial against a 429ing endpoint carries a typed
			// retry_after (connectLocked / rateLimitFromUpgrade). Falling
			// through to our own doubling schedule here would poll TIGHTER
			// than the server just asked -- the #443 shape, where a tight
			// retry loop burned a node's daily Redis-API quota and locked it
			// out for about a day. Honor the server's interval instead, in
			// done-aware chunks so a shutdown still interrupts the sleep
			// (mirrors cmd/work.go's connectWithBackoffLabeled), and reset the
			// local schedule afterwards: once the server has spoken, it is the
			// authority, not wherever our own doubling had gotten to.
			if rle, ok := AsRateLimitError(err); ok {
				if wait := rle.Wait(time.Now()); wait > 0 {
					c.debug("ws: reconnect: rate limited (limit=%d window=%q), honoring server backoff of %s instead of local schedule", rle.Limit, rle.Window, wait)
					if !c.sleepUnlessDoneUntil(time.Now().Add(wait)) {
						return
					}
					c.connMu.Lock()
					c.reconnectBackoff = initialReconnectBackoff
					c.connMu.Unlock()
				}
			}
			continue
		}

		c.debug("ws: reconnected successfully")
		c.fireReconnectCallbacks()
		return
	}
}

// Subscribe subscribes to one or more channels.
func (c *WSClient) Subscribe(ctx context.Context, channels ...string) error {
	if len(channels) == 0 {
		return nil
	}

	// Track subscriptions for reconnect
	c.subscriptionsMu.Lock()
	for _, ch := range channels {
		c.subscriptions[ch] = true
	}
	c.subscriptionsMu.Unlock()

	msg := WSMessage{
		Type:     "subscribe",
		Channels: channels,
	}

	return c.sendMessage(ctx, msg)
}

// Unsubscribe unsubscribes from one or more channels.
func (c *WSClient) Unsubscribe(ctx context.Context, channels ...string) error {
	if len(channels) == 0 {
		return nil
	}

	// Remove from tracked subscriptions
	c.subscriptionsMu.Lock()
	for _, ch := range channels {
		delete(c.subscriptions, ch)
	}
	c.subscriptionsMu.Unlock()

	msg := WSMessage{
		Type:     "unsubscribe",
		Channels: channels,
	}

	return c.sendMessage(ctx, msg)
}

// Publish publishes a message to a channel.
func (c *WSClient) Publish(ctx context.Context, channel string, message any) error {
	// Convert message to map if needed
	var msgMap map[string]any
	switch m := message.(type) {
	case map[string]any:
		msgMap = m
	default:
		// Marshal and unmarshal to convert struct to map
		data, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}
		if err := json.Unmarshal(data, &msgMap); err != nil {
			return fmt.Errorf("failed to convert message to map: %w", err)
		}
	}

	msg := WSMessage{
		Type:    "publish",
		Channel: channel,
		Message: msgMap,
	}

	return c.sendMessage(ctx, msg)
}

// sendMessage sends a message over the WebSocket
func (c *WSClient) sendMessage(_ context.Context, msg WSMessage) error {
	c.connMu.RLock()
	conn := c.conn
	connected := c.connected
	c.connMu.RUnlock()

	if !connected || conn == nil {
		return fmt.Errorf("WebSocket not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	c.debug("ws: sending message type=%s", msg.Type)

	// Single-writer discipline (#720). Callers are genuinely concurrent: the
	// heartbeat publisher ticks every 30s, job streaming publishes per chunk,
	// the consume loop acks per job, and reconnect re-subscribes from its own
	// goroutine. Without this lock they all enter WriteMessage at once.
	//
	// The deadline is set AND cleared inside the same critical section:
	// SetWriteDeadline is one of gorilla's write methods (a bare field
	// assignment on the Conn), so touching it outside writeMu would race an
	// in-flight WriteMessage, which is the bug this fix found in Close.
	c.writeMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	err = conn.WriteMessage(websocket.TextMessage, data)
	_ = conn.SetWriteDeadline(time.Time{})
	c.writeMu.Unlock()

	if err != nil {
		// gorilla latches the first write error (Conn.writeErr, set once by
		// writeFatal) and returns it from every later write, so this
		// connection can never carry another byte. Tear it down: that flips
		// IsConnected to false so Client.Publish falls back to HTTP, and it
		// kicks reconnect. Without it the socket stays "connected" while
		// every publish fails forever, which is the silent stall shape this
		// whole fix exists to avoid.
		c.handleDisconnect(conn)
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// closeWriteWait bounds both the close-handshake write and how long Close
// waits for an in-flight write to finish before giving up on the handshake.
const closeWriteWait = 2 * time.Second

// defaultWriteTimeout bounds a single WriteMessage.
//
// 15s is chosen against the slowest-cadence caller rather than against network
// latency: the heartbeat publishes every 30s, so a 15s bound guarantees a
// wedged socket surfaces as an error before the next tick can queue behind it,
// and at most one heartbeat interval is ever affected. It is also far above any
// plausible time to hand a few-KB frame to the kernel on a healthy connection,
// so it should never fire in normal operation, and well under the worker
// self-heal stall timer (600s) that would otherwise be the only backstop.
const defaultWriteTimeout = 15 * time.Second

// Keepalive intervals (#734).
//
// defaultPongWait is the read deadline. Nothing arriving on the socket within
// it -- not a message, not a pong for our own pings -- means the connection is
// declared dead and torn down. It is sized against the platform's 120s offline
// sweep, not against network latency: detection has to happen with room to
// spare, or the node is already marked offline (and refused for targeted
// dispatch) by the time it notices. 45s detection plus ~1s of reconnect backoff
// leaves better than 2x margin, and lands inside two 30s heartbeat intervals.
//
// defaultPingInterval is one third of it on purpose, so three pings go
// unanswered before we act. That absorbs a single dropped pong and scheduler
// jitter on a GPU-saturated node without pushing detection past the sweep.
// Tightening these buys little (the sweep, not us, sets the deadline that
// matters) and costs false-positive teardowns of working connections.
const (
	defaultPingInterval = 15 * time.Second
	defaultPongWait     = 45 * time.Second
)

// pingWriteWait bounds the ping control frame itself. WriteControl blocks until
// it can take gorilla's write semaphore, which an in-flight WriteMessage holds
// for up to defaultWriteTimeout, so this is a bound on how long a ping may wait
// rather than a liveness signal. Failing here is advisory; see pingLoop.
const pingWriteWait = 10 * time.Second

// pongReplyWait bounds the pong we send in answer to a server ping. It is much
// shorter than pingWriteWait because that reply runs on the read goroutine; see
// the ping handler in armKeepalive. It matches gorilla's own default handler.
const pongReplyWait = time.Second

// tryLockWrite acquires writeMu, giving up after d. Close must not block
// forever behind a wedged writer (see the deadline note in Close), so it needs
// a bounded acquire rather than a plain Lock.
func (c *WSClient) tryLockWrite(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if c.writeMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// OnMessage registers a handler for a specific message type.
// Use "*" to handle all message types.
func (c *WSClient) OnMessage(msgType string, handler func(WSMessage)) {
	c.handlersMu.Lock()
	c.handlers[msgType] = handler
	c.handlersMu.Unlock()
}

// OnReconnect registers a callback that fires after a successful reconnection.
// The callback is NOT called on the initial Connect -- only on reconnects.
// Callbacks run outside of the connection lock so it is safe to call sendMessage.
func (c *WSClient) OnReconnect(callback func()) {
	c.reconnectCallbacksMu.Lock()
	c.reconnectCallbacks = append(c.reconnectCallbacks, callback)
	c.reconnectCallbacksMu.Unlock()
}

// StartConsume sends a consume message to begin receiving jobs from the server.
// The server will start a persistent XREADGROUP BLOCK loop and push job messages.
func (c *WSClient) StartConsume(queues []string, group, consumer string, count, blockMs int) error {
	msg := WSMessage{
		Type:     "consume",
		Queues:   queues,
		Group:    group,
		Consumer: consumer,
		Count:    count,
		BlockMs:  blockMs,
	}
	return c.sendMessage(context.Background(), msg)
}

// StopConsume sends a stop_consume message to halt the server-side consume loop.
func (c *WSClient) StopConsume() error {
	msg := WSMessage{
		Type: "stop_consume",
	}
	return c.sendMessage(context.Background(), msg)
}

// AckJob sends an ack message to acknowledge a processed job.
func (c *WSClient) AckJob(queue, group, messageID string) error {
	msg := WSMessage{
		Type:      "ack",
		Queue:     queue,
		Group:     group,
		MessageID: messageID,
	}
	return c.sendMessage(context.Background(), msg)
}

// IsConnected returns whether the WebSocket is currently connected.
func (c *WSClient) IsConnected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.connected
}

// Close closes the WebSocket connection.
func (c *WSClient) Close() error {
	c.stopOnce.Do(func() {
		close(c.done)
	})

	c.connMu.Lock()
	defer c.connMu.Unlock()

	c.connected = false
	if c.conn != nil {
		// Bound the close-handshake write: WriteMessage has no deadline by
		// default, so on a half-dead connection (e.g. after macOS sleep or a
		// network change) it can block indefinitely. That stalls shutdown via
		// chat.Client.Close() (issue #312). A short deadline makes the write
		// fail fast; we then close the underlying TCP conn regardless.
		//
		// SetWriteDeadline is itself one of gorilla's write methods (it is a
		// bare field assignment on the Conn), so it and the close frame both
		// have to run under writeMu, otherwise shutdown races an in-flight
		// sendMessage. The acquire is bounded so a wedged writer cannot
		// reintroduce the #312 stall; if we cannot get the lock we skip the
		// courtesy close frame entirely. Closing the connection is documented
		// safe alongside any other method, and it unblocks the stuck writer.
		if c.tryLockWrite(closeWriteWait) {
			_ = c.conn.SetWriteDeadline(time.Now().Add(closeWriteWait))
			err := c.conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			)
			c.writeMu.Unlock()
			if err != nil {
				c.debug("ws: error sending close message: %v", err)
			}
		} else {
			c.debug("ws: write in flight, closing without close handshake")
		}
		return c.conn.Close()
	}

	return nil
}
