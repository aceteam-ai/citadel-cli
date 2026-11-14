// Bounded, single-connection co-browse actions with partial-effect reporting
// (citadel-cli#1663, node side of aceteam#13495).
//
// Before this file every CDP call of an action paid for its own /json
// discovery plus a fresh WebSocket (a selector click made four of each, and the
// job handler's status decoration a fifth discovery). An action now opens ONE
// connection and runs all of its calls over it, and it carries its own deadline.
// When the deadline passes, or a call fails after the action began acting, the
// caller gets an *ActionIncompleteError that says what actually happened
// (scrolled, focused, clicked, typed) so it knows its screenshot coordinates may
// be stale. Nothing here retries: a partly done action is reported, never
// repeated.
package platform

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// CobrowseActionDeadline bounds one scripted action (click, type, key, scroll,
// screenshot) inside the node. The platform waits 15s for a job result, so the
// action must give up, and still report, before that. The budget is
// CobrowseActionDeadline (acting) + cobrowseReportReserve (reading back what
// happened) = 11s, leaving 4s of the platform's 15s for queueing and the relay.
// It is a variable only so tests need not sleep.
var CobrowseActionDeadline = 9 * time.Second

// cobrowseReportReserve is the extra time, after the deadline, spent reading
// back scroll offset, focus and URL for the partial-effect report.
var cobrowseReportReserve = 2 * time.Second

// cobrowseStatusProbeTimeout bounds the /json probe status() performs. A wedged
// DevTools endpoint falls back to the last URL seen instead of stalling a
// status call for the full discovery timeout.
var cobrowseStatusProbeTimeout = 2 * time.Second

// ActionEffects is what an action did to the page. It is reported on success
// and, more importantly, on a deadline or failure.
type ActionEffects struct {
	// Dispatched is true once an input event (mouse, key, text, wheel) was
	// sent to the browser. A dispatched action must not be repeated blindly.
	Dispatched bool `json:"dispatched"`
	// Scrolled is true when the scroll offset changed; ScrollX/ScrollY are the
	// offset now, so a caller can tell its earlier coordinates are stale.
	Scrolled bool    `json:"scrolled"`
	ScrollX  float64 `json:"scroll_x"`
	ScrollY  float64 `json:"scroll_y"`
	// Focused is true when focus moved to an element (never body); FocusTag is
	// its tag name only, never its content.
	Focused  bool   `json:"focused"`
	FocusTag string `json:"focus_tag,omitempty"`
	// Clicked is true once the mouse press and release were both sent.
	Clicked bool `json:"clicked"`
	// TypedLength is the length in characters of text sent to the browser;
	// never the text. TypedUnconfirmed is true when it was sent but no
	// acknowledgement arrived before the deadline.
	TypedLength      int  `json:"typed_length"`
	TypedUnconfirmed bool `json:"typed_unconfirmed,omitempty"`
	// Navigated is true when the page URL differs from before the action.
	Navigated bool `json:"navigated"`
}

// anyEffect reports whether the page may have changed.
func (e ActionEffects) anyEffect() bool {
	return e.Dispatched || e.Scrolled || e.Focused || e.Clicked || e.TypedLength > 0 || e.Navigated
}

// ActionIncompleteError is returned when an action hit its deadline or failed
// after it may already have acted. Effects says what happened; Status is the
// session status read back (URL included).
type ActionIncompleteError struct {
	Action   string
	TimedOut bool
	Effects  ActionEffects
	Status   CobrowseSessionStatus
	Cause    error
}

func (e *ActionIncompleteError) Error() string {
	reason := "failed"
	if e.TimedOut {
		reason = "hit its time limit"
	}
	var parts []string
	if e.Effects.Dispatched {
		parts = append(parts, "input sent")
	}
	if e.Effects.Scrolled {
		parts = append(parts, fmt.Sprintf("scrolled to x=%g y=%g", e.Effects.ScrollX, e.Effects.ScrollY))
	}
	if e.Effects.Focused {
		parts = append(parts, "focused a "+e.Effects.FocusTag)
	}
	if e.Effects.Clicked {
		parts = append(parts, "clicked")
	}
	if e.Effects.TypedLength > 0 {
		parts = append(parts, fmt.Sprintf("typed %d characters", e.Effects.TypedLength))
	}
	effects := "no page change seen"
	if len(parts) > 0 {
		effects = strings.Join(parts, ", ")
	}
	msg := fmt.Sprintf("%s %s: %s. Not retried; take a fresh screenshot before acting by coordinates", e.Action, reason, effects)
	if e.Cause != nil && !e.TimedOut {
		msg = e.Cause.Error() + " (" + msg + ")"
	}
	return msg
}

func (e *ActionIncompleteError) Unwrap() error { return e.Cause }

// Reportable reports whether the failure should be returned as a structured
// result: the page may have changed, or the deadline passed with the outcome
// unknown. A failure that changed nothing stays a plain error.
func (e *ActionIncompleteError) Reportable() bool { return e.TimedOut || e.Effects.anyEffect() }

// AsActionIncomplete extracts an *ActionIncompleteError from err.
func AsActionIncomplete(err error) (*ActionIncompleteError, bool) {
	var ae *ActionIncompleteError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// cdpCaller sends CDP calls. portCaller opens a connection per call (the
// original behavior, kept for helpers whose callers are not budgeted);
// *cdpConn reuses one connection.
type cdpCaller interface {
	Call(method string, params map[string]any) (map[string]any, error)
}

type portCaller int

func (p portCaller) Call(method string, params map[string]any) (map[string]any, error) {
	return cdpCommand(int(p), method, params)
}

// cdpConn is one discovered page target and one WebSocket, used for all the
// calls of a single action.
type cdpConn struct {
	ws     *websocket.Conn
	ctx    context.Context
	target cdpTarget
	stop   chan struct{}
	once   sync.Once
	nextID int
}

// openCDPConn discovers the page target once and dials it once.
func openCDPConn(ctx context.Context, port int) (*cdpConn, error) {
	target, err := pickTargetContext(ctx, port)
	if err != nil {
		return nil, err
	}
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("CDP dial: %w", err)
	}
	c := &cdpConn{ws: ws, ctx: ctx, target: target, stop: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-c.stop:
		}
	}()
	return c, nil
}

func (c *cdpConn) Close() {
	c.once.Do(func() {
		close(c.stop)
		_ = c.ws.Close()
	})
}

// Call sends one method and waits for its response, bounded by the
// connection's context.
func (c *cdpConn) Call(method string, params map[string]any) (map[string]any, error) {
	if params == nil {
		params = map[string]any{}
	}
	c.nextID++
	id := c.nextID
	_ = c.ws.SetWriteDeadline(contextDeadlineOr(c.ctx, 10*time.Second))
	if err := c.ws.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		if ce := c.ctx.Err(); ce != nil {
			return nil, fmt.Errorf("CDP write: %w", ce)
		}
		return nil, fmt.Errorf("CDP write: %w", err)
	}
	_ = c.ws.SetReadDeadline(contextDeadlineOr(c.ctx, 15*time.Second))
	for {
		var msg map[string]any
		if err := c.ws.ReadJSON(&msg); err != nil {
			if ce := c.ctx.Err(); ce != nil {
				return nil, fmt.Errorf("CDP read: %w", ce)
			}
			return nil, fmt.Errorf("CDP read: %w", err)
		}
		idv, ok := msg["id"]
		if !ok || fmt.Sprint(idv) != fmt.Sprint(id) {
			continue
		}
		if e, ok := msg["error"]; ok {
			return nil, fmt.Errorf("CDP error: %v", e)
		}
		if res, ok := msg["result"].(map[string]any); ok {
			return res, nil
		}
		return map[string]any{}, nil
	}
}

// evaluateOn is runtimeEvaluateWithPromise over any caller.
func evaluateOn(c cdpCaller, expr string, awaitPromise bool) (any, error) {
	res, err := c.Call("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
	})
	if err != nil {
		return nil, err
	}
	if exc, ok := res["exceptionDetails"]; ok && exc != nil {
		return nil, fmt.Errorf("script error: %v", exc)
	}
	remote, ok := res["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("evaluate returned no result")
	}
	return remote["value"], nil
}

// pageProbe is the cheap read-back used before and after an action.
type pageProbe struct {
	url        string
	x, y       float64
	focusTag   string
	focusSig   string
	probeValid bool
}

// effectProbeScript reads scroll offset, URL and the focused element's tag and
// a position signature. It reads no element content.
const effectProbeScript = `(function(){
	var a = document.activeElement;
	while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement;
	var tag = "", sig = "";
	if (a && a !== document.body && a !== document.documentElement) {
		var r = a.getBoundingClientRect();
		tag = (a.tagName || "").toLowerCase();
		sig = [tag, a.id || "", a.name || "", a.type || "", Math.round(r.x), Math.round(r.y), Math.round(r.width), Math.round(r.height)].join("|");
	}
	return {url: location.href, x: window.scrollX, y: window.scrollY, tag: tag, sig: sig};
})()`

func probePage(c cdpCaller) pageProbe {
	val, err := evaluateOn(c, effectProbeScript, false)
	obj, ok := val.(map[string]any)
	if err != nil || !ok {
		return pageProbe{}
	}
	p := pageProbe{probeValid: true}
	p.url, _ = obj["url"].(string)
	p.x, _ = obj["x"].(float64)
	p.y, _ = obj["y"].(float64)
	p.focusTag, _ = obj["tag"].(string)
	p.focusSig, _ = obj["sig"].(string)
	return p
}

// applyProbe fills the observable effects from a before/after pair.
func applyProbe(eff *ActionEffects, before, after pageProbe) {
	if !after.probeValid {
		return
	}
	eff.ScrollX, eff.ScrollY = after.x, after.y
	if before.probeValid {
		eff.Scrolled = math.Abs(after.x-before.x) >= 0.5 || math.Abs(after.y-before.y) >= 0.5
		eff.Navigated = before.url != "" && after.url != "" && before.url != after.url
		eff.Focused = after.focusSig != "" && after.focusSig != before.focusSig
	} else {
		eff.Focused = after.focusSig != ""
	}
	if eff.Focused {
		eff.FocusTag = after.focusTag
	}
}

// actionRun is the handle an action body uses.
type actionRun struct {
	conn   cdpCaller
	eff    *ActionEffects
	before pageProbe
}

// runAction runs body over one connection under CobrowseActionDeadline and
// returns the session status with Effects and Done set. On deadline or failure
// it returns *ActionIncompleteError. A failure before the connection opened
// (nothing was sent) is a plain error.
func (s *cobrowseSession) runAction(port int, action string, body func(*actionRun) error) (CobrowseSessionStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), CobrowseActionDeadline)
	defer cancel()
	conn, err := openCDPConn(ctx, port)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	defer conn.Close()

	eff := ActionEffects{}
	run := &actionRun{conn: conn, eff: &eff}
	run.before = probePage(conn)
	bodyErr := body(run)

	if bodyErr == nil {
		after := probePage(conn)
		applyProbe(&eff, run.before, after)
		st := s.statusWithURL(after.url, conn.target.URL)
		done := true
		st.Done, st.Effects = &done, &eff
		return st, nil
	}

	timedOut := ctx.Err() != nil || errors.Is(bodyErr, context.DeadlineExceeded)
	// Read back what happened on a fresh, short connection: the action's own
	// context may be spent and its socket closed.
	rctx, rcancel := context.WithTimeout(context.Background(), cobrowseReportReserve)
	defer rcancel()
	var after pageProbe
	if rc, rerr := openCDPConn(rctx, port); rerr == nil {
		after = probePage(rc)
		rc.Close()
	}
	applyProbe(&eff, run.before, after)
	st := s.statusWithURL(after.url, "")
	done := false
	st.Done, st.Effects = &done, &eff
	return st, &ActionIncompleteError{Action: action, TimedOut: timedOut, Effects: eff, Status: st, Cause: bodyErr}
}

// statusWithURL is status() without the discovery probe: the caller already
// read the URL over its own connection.
func (s *cobrowseSession) statusWithURL(urls ...string) CobrowseSessionStatus {
	st := s.statusNoProbe()
	for _, u := range urls {
		if u != "" {
			st.URL = u
			s.mu.Lock()
			s.lastURL = u
			s.mu.Unlock()
			return st
		}
	}
	s.mu.Lock()
	st.URL = s.lastURL
	s.mu.Unlock()
	return st
}

// typedLength counts the characters sent, for the report.
func typedLength(text string) int { return utf8.RuneCountInString(text) }
