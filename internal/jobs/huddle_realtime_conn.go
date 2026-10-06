// internal/jobs/huddle_realtime_conn.go
//
// Production realtimeConn: a gorilla/websocket client to the AceTeam realtime
// engine (wss://<api>/api/realtime), used by the HUDDLE_JOIN converse bridge.
//
// Auth: the huddle mint response is itself a short-lived `act_` key for the
// agent's author and is accepted by the realtime WebSocket's authenticate() path.
// Reusing it preserves one least-privilege identity and avoids a global secret or
// caller-supplied destination/credential. The endpoint is always derived from the
// enrolled API origin that minted the key.
//
// Turn detection: `turnDetection=server_vad` rides the URL query so the engine
// runs server-side VAD (the bridge never forces a turn with commit/response.create).
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// realtimeDialTimeout bounds the WS handshake (not the session).
	realtimeDialTimeout = 20 * time.Second
	// The realtime protocol carries JSON control events plus base64 PCM chunks.
	// One MiB is far above a normal audio delta while preventing a peer from
	// making gorilla allocate an unbounded message before recvLoop can validate
	// its decoded PCM payload.
	realtimeWSMessageMaxBytes int64 = 1 << 20
)

// gorillaRealtimeConn adapts a *websocket.Conn to the realtimeConn seam. gorilla
// allows a single READER and a single WRITER at a time. recvLoop is the only
// reader, but TWO goroutines call Send (hearLoop's append and speakerLoop's
// clear), so writes MUST be serialized here — writeMu does that (gorilla panics
// with "concurrent write to websocket connection" otherwise).
type gorillaRealtimeConn struct {
	c       *websocket.Conn
	writeMu sync.Mutex
}

func (g *gorillaRealtimeConn) Send(payload []byte) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	return g.c.WriteMessage(websocket.TextMessage, payload)
}

func (g *gorillaRealtimeConn) Recv() ([]byte, error) {
	_, data, err := g.c.ReadMessage()
	return data, err
}

func (g *gorillaRealtimeConn) Close() error {
	return g.c.Close()
}

// dialRealtime builds the realtime WS URL from the API base (or an explicit
// override) and dials it with the Bearer token. agentID is passed as a query param
// so the engine resolves the right agent session; turnDetection=server_vad opts
// into hands-free turn taking.
func dialRealtime(ctx context.Context, apiBase *url.URL, token, agentID string) (realtimeConn, error) {
	wsURL, err := realtimeWSURL(apiBase, agentID)
	if err != nil {
		return nil, err
	}
	dialer := &websocket.Dialer{HandshakeTimeout: realtimeDialTimeout}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	c, resp, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("dial realtime ws (status %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dial realtime ws: %w", err)
	}
	c.SetReadLimit(realtimeWSMessageMaxBytes)
	// DialContext only covers the HTTP upgrade. ReadMessage below can otherwise
	// remain blocked until the readiness deadline even after the job is cancelled.
	// Closing a gorilla connection is safe concurrently with a read and wakes that
	// read immediately. Stop and join the watcher before every return so it cannot
	// race with a successfully returned live connection or leak after this phase.
	readinessWatchStop := make(chan struct{})
	readinessWatchDone := make(chan struct{})
	go func() {
		defer close(readinessWatchDone)
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-readinessWatchStop:
		}
	}()
	watchStopped := false
	stopReadinessWatch := func() {
		if watchStopped {
			return
		}
		close(readinessWatchStop)
		<-readinessWatchDone
		watchStopped = true
	}
	defer stopReadinessWatch()

	// HTTP 101 is not authentication success: the websocket wrapper authenticates
	// asynchronously and emits `connected` only after the bearer is accepted.
	// That is still NOT media readiness. The realtime route emits
	// `connection_ready` only after the upstream model socket and session config
	// are ready to consume audio. Do not start capture before both boundaries or
	// the first room frames can be silently dropped while OpenAI is CONNECTING.
	_ = c.SetReadDeadline(time.Now().Add(realtimeDialTimeout))
	authenticated := false
	for {
		_, raw, readErr := c.ReadMessage()
		if readErr != nil {
			_ = c.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("await realtime readiness: %w", ctxErr)
			}
			return nil, fmt.Errorf("await realtime readiness: %w", readErr)
		}
		var event struct {
			Type   string `json:"type"`
			Config struct {
				VoiceCloneID *string `json:"voiceCloneId"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("realtime readiness returned malformed JSON")
		}
		switch event.Type {
		case "connected":
			authenticated = true
		case "connection_ready":
			if !authenticated {
				_ = c.Close()
				return nil, fmt.Errorf("realtime became ready before authentication acknowledgement")
			}
			// Voice-clone providers currently emit compressed audio (for example
			// ElevenLabs MP3), while meetingd's low-latency endpoint accepts raw
			// PCM16. Fail before capture rather than play compressed bytes as PCM
			// noise or report a silent successful conversation.
			if event.Config.VoiceCloneID != nil && *event.Config.VoiceCloneID != "" {
				_ = c.Close()
				return nil, fmt.Errorf("realtime agent uses compressed voice-clone audio; huddle converse requires PCM16 output")
			}
			stopReadinessWatch()
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = c.Close()
				return nil, fmt.Errorf("await realtime readiness: %w", ctxErr)
			}
			_ = c.SetReadDeadline(time.Time{})
			return &gorillaRealtimeConn{c: c}, nil
		case "error":
			_ = c.Close()
			return nil, fmt.Errorf("realtime server reported an error before media readiness")
		case "reconnecting":
			_ = c.Close()
			return nil, fmt.Errorf("realtime server started reconnecting before media readiness")
		}
	}
}

// realtimeWSURL derives the endpoint exclusively from the trusted enrolled API
// origin. Payload data cannot redirect the minted credential.
func realtimeWSURL(apiBase *url.URL, agentID string) (string, error) {
	if apiBase == nil {
		return "", fmt.Errorf("missing enrolled API origin")
	}
	u := *apiBase
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
		// already a ws scheme
	default:
		return "", fmt.Errorf("unsupported realtime url scheme %q", u.Scheme)
	}
	u.Path = "/api/realtime"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	q := u.Query()
	if agentID != "" && q.Get("agentId") == "" {
		q.Set("agentId", agentID)
	}
	if q.Get("turnDetection") == "" {
		q.Set("turnDetection", "server_vad")
	}
	// The realtime route's durable session/transcript owner is a human user.
	// A huddle participant is the bound agent, so never persist this room audio
	// or transcript under the author's user identity.
	q.Set("incognito", "true")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

var _ realtimeConn = (*gorillaRealtimeConn)(nil)
