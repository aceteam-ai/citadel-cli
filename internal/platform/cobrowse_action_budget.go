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
	"net"
	"os"
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
	// Dispatched is true when an input event (mouse, key, text, wheel) may
	// have been sent to the browser: it was written, and no refusal came back.
	// A dispatched action must not be repeated blindly.
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
	// FilesSet is the number of files handed to a file input by set_file_input.
	FilesSet int `json:"files_set,omitempty"`
	// Navigated is true when the page URL differs from before the action.
	Navigated bool `json:"navigated"`
	// Unknown is true when the read-back could not run (the page was refused,
	// no longer active, or did not answer): scroll, focus and navigation are
	// then not reported, not reported as "no change".
	Unknown bool `json:"effects_unknown,omitempty"`
}

// anyEffect reports whether the page is known or likely to have changed.
// Dispatched alone is not an effect: it only says input may have been sent.
func (e ActionEffects) anyEffect() bool {
	return e.Scrolled || e.Focused || e.Clicked || e.TypedLength > 0 || e.Navigated || e.FilesSet > 0
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
		parts = append(parts, "input may have been sent")
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
	if e.Effects.FilesSet > 0 {
		parts = append(parts, fmt.Sprintf("set %d file(s) on a file input", e.Effects.FilesSet))
	}
	if e.Effects.TypedLength > 0 {
		unconfirmed := ""
		if e.Effects.TypedUnconfirmed {
			unconfirmed = " (unconfirmed)"
		}
		parts = append(parts, fmt.Sprintf("typed %d characters%s", e.Effects.TypedLength, unconfirmed))
	}
	if e.Effects.Unknown {
		parts = append(parts, "page state could not be read back")
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
func (e *ActionIncompleteError) Reportable() bool {
	return e.Effects.anyEffect() || e.Effects.Dispatched
}

// isActionRefusal reports whether err is a refusal that must reach the caller
// as itself, never wrapped as a partial effect: nothing was sent.
func isActionRefusal(err error) bool {
	for _, r := range []error{ErrHandedOff, ErrNotStarted, ErrTargetChanged, ErrPageRefused} {
		if errors.Is(err, r) {
			return true
		}
	}
	return false
}

// AsActionIncomplete extracts an *ActionIncompleteError from err.
func AsActionIncomplete(err error) (*ActionIncompleteError, bool) {
	var ae *ActionIncompleteError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// cdpCaller sends CDP calls. A pin connects per call (the original behavior,
// kept for helpers whose callers are not budgeted); *cdpConn reuses one
// connection to the pinned page.
type cdpCaller interface {
	Call(method string, params map[string]any) (map[string]any, error)
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
	// readCap, when set, bounds one call below the connection's own deadline.
	readCap time.Duration
	// failed is set when a call failed at the transport; the socket is then
	// unusable (a read deadline poisons it) and must be replaced.
	failed bool
}

// openCDPConn dials the page an action is pinned to. With page tracking the pin
// already names that page's debugger socket (resolved by pinAction, so
// ErrTargetChanged and ErrPageRefused are decided before any connection), and
// no discovery happens here. Without tracking the first page is discovered once.
func openCDPConn(ctx context.Context, p pin) (*cdpConn, error) {
	var target cdpTarget
	target.WebSocketDebuggerURL = p.ws
	if p.ws == "" {
		var err error
		target, err = pickTargetContext(ctx, p.port)
		if err != nil {
			return nil, err
		}
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
		c.failed = true
		if ce := c.ctx.Err(); ce != nil {
			return nil, fmt.Errorf("CDP write: %w", ce)
		}
		return nil, fmt.Errorf("CDP write: %w", err)
	}
	readFor := 15 * time.Second
	if c.readCap > 0 {
		readFor = c.readCap
	}
	_ = c.ws.SetReadDeadline(contextDeadlineOr(c.ctx, readFor))
	for {
		var msg map[string]any
		if err := c.ws.ReadJSON(&msg); err != nil {
			c.failed = true
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
	s      *cobrowseSession
	p      pin
	ctx    context.Context
	conn   *cdpConn
	eff    *ActionEffects
	before pageProbe
	// pressed is true between a mousePressed being sent and its mouseReleased
	// being acknowledged; px, py are where.
	pressed bool
	px, py  float64
}

// beforeProbeLimit bounds the pre-action read so a slow page cannot eat the
// action's own budget; on timeout the action proceeds with effects unknown.
var beforeProbeLimit = 1500 * time.Millisecond

// actionBudget is one action's whole time allowance, created before the page is
// pinned so pinning, verification, target syncs, probes and the status read-back
// all spend the same budget. op covers the action plus the report reserve; act
// is the acting part and ends CobrowseActionDeadline after op began.
type actionBudget struct {
	op, act context.Context
}

func newActionBudget() (actionBudget, context.CancelFunc) {
	op, cancelOp := context.WithTimeout(context.Background(), CobrowseActionDeadline+cobrowseReportReserve)
	act, cancelAct := context.WithTimeout(op, CobrowseActionDeadline)
	return actionBudget{op: op, act: act}, func() { cancelAct(); cancelOp() }
}

// pageActiveErr reports nil only when, with page tracking, the pinned page is
// still the active, policy-approved one. It lists pages (no evaluation), and it
// also lets the target set close or refuse a page that navigated somewhere
// refused, so nothing evaluates in a page that has not passed policy. Without
// tracking there is no policy and the page is trusted as before. A page that is
// not the active one refuses with ErrTargetChanged, or ErrPageRefused when no
// page is admitted.
func pageActiveErr(ctx context.Context, p pin) error {
	if p.set == nil {
		return nil
	}
	snap, err := p.set.sync(ctx, p.port)
	if err != nil {
		return err
	}
	if snap.Active == "" {
		return ErrPageRefused
	}
	if snap.Active != p.id {
		return ErrTargetChanged
	}
	return nil
}

func pageActive(ctx context.Context, p pin) bool { return pageActiveErr(ctx, p) == nil }

// fence is the final check before the first input event or capture: the live
// process is still running and not handed to a human, and the pinned page is
// still the active, admitted one. It must come after the last awaited
// preparation, so nothing slow sits between it and the dispatch.
func (s *cobrowseSession) fence(ctx context.Context, p pin) error {
	// The page check awaits discovery, so it runs first; the driver check is
	// instant and sits last, immediately before the dispatch.
	if err := pageActiveErr(ctx, p); err != nil {
		return err
	}
	return s.driverFence(p)
}

// driverFence refuses unless the live process the pin was taken on is still
// running and not handed to a human.
func (s *cobrowseSession) driverFence(p pin) error {
	port, err := s.requireDrivablePort()
	if err != nil {
		return err
	}
	if port != p.port {
		return ErrNotStarted
	}
	return nil
}

func (r *actionRun) fence() error { return r.s.fence(r.ctx, r.p) }

// runAction runs body over one connection under the action budget and returns
// the session status with Effects and Done set. On deadline or failure it
// returns *ActionIncompleteError when the page is known or likely to have
// changed, or input may have been dispatched; any other failure, and every
// refusal, is returned unchanged.
func (s *cobrowseSession) runAction(a actionBudget, p pin, action string, body func(*actionRun) error) (CobrowseSessionStatus, error) {
	ctx := a.act
	conn, err := openCDPConn(ctx, p)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	defer func() { conn.Close() }()

	eff := ActionEffects{}
	run := &actionRun{s: s, p: p, ctx: ctx, conn: conn, eff: &eff}
	// A page that is not the active, admitted one gets no probe and no action.
	if err := pageActiveErr(ctx, p); err != nil {
		return CobrowseSessionStatus{}, err
	}
	conn.readCap = beforeProbeLimit
	run.before = probePage(conn)
	conn.readCap = 0
	if !run.before.probeValid && conn.failed {
		// The poisoned socket is replaced; the action proceeds with the
		// before-state unknown.
		conn.Close()
		nc, err := openCDPConn(ctx, p)
		if err != nil {
			return CobrowseSessionStatus{}, err
		}
		conn, run.conn = nc, nc
	}
	bodyErr := body(run)

	if bodyErr == nil {
		after, rbErr := readBack(a.op, p, run, conn)
		if rbErr != nil && eff.Dispatched {
			// The input went out, then the agent lost control. The page is
			// not read during human control, so the outcome is unknown.
			eff.Unknown = true
			ae := &ActionIncompleteError{Action: action, Effects: eff, Cause: errReadBackNoDriver}
			st := s.statusWithURL(a.op)
			done := false
			st.Done, st.Effects = &done, &eff
			ae.Status = st
			return st, ae
		}
		applyProbe(&eff, run.before, after)
		eff.Unknown = !after.probeValid
		st := s.statusWithURL(a.op, after.url, conn.target.URL)
		done := true
		st.Done, st.Effects = &done, &eff
		return st, nil
	}
	if isActionRefusal(bodyErr) {
		return CobrowseSessionStatus{}, bodyErr
	}

	timedOut := ctx.Err() != nil || isTimeoutErr(bodyErr)
	// Read back what happened on a fresh connection: the action's own context
	// may be spent and its socket closed. It spends the report reserve of the
	// same budget.
	var after pageProbe
	if rc, rerr := openCDPConn(a.op, p); rerr == nil {
		// A lost driver leaves after empty; a dispatched action is then
		// reported with unknown effects below.
		after, _ = readBack(a.op, p, run, rc)
		rc.Close()
	}
	applyProbe(&eff, run.before, after)
	eff.Unknown = !after.probeValid
	if eff.Dispatched {
		// Whatever the error was, the input may have landed: the effects are
		// not known, and the caller must not repeat the action blindly.
		eff.Unknown = true
	}
	ae := &ActionIncompleteError{Action: action, TimedOut: timedOut, Effects: eff, Cause: bodyErr}
	if !ae.Reportable() {
		return CobrowseSessionStatus{}, bodyErr
	}
	st := s.statusWithURL(a.op, after.url, "")
	done := false
	st.Done, st.Effects = &done, &eff
	ae.Status = st
	return st, ae
}

// isTimeoutErr reports a deadline of any kind: the action context's, or the
// socket's own read or write deadline (which is set from it and can fire first).
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout())
}

// readBack reads the page state over c, but only while the pinned page is still
// the active, approved one and the agent still drives: it sends a best-effort
// mouseReleased first when a click timed out between press and release, and
// never evaluates in a page that is refused or no longer active, or while a
// human has control. A lost driver (handoff or stop) is returned as an error so
// a dispatched action reports its outcome as unknown; a page that is simply no
// longer active (a click opened a popup) reads back nothing, without error.
func readBack(ctx context.Context, p pin, run *actionRun, c *cdpConn) (pageProbe, error) {
	if !pageActive(ctx, p) {
		return pageProbe{}, nil
	}
	// The page check awaits discovery; the driver check is instant and sits
	// last, immediately before anything is sent to the page.
	if err := run.s.driverFence(p); err != nil {
		return pageProbe{}, err
	}
	if run.pressed {
		_, _ = c.Call("Input.dispatchMouseEvent", map[string]any{
			"type": "mouseReleased", "x": run.px, "y": run.py, "button": "left", "clickCount": 1,
		})
		run.pressed = false
		// The release is awaited, so a human can take control during it.
		if err := run.s.driverFence(p); err != nil {
			return pageProbe{}, err
		}
	}
	return probePage(c), nil
}

// errDriverLostMidAction is the cause when part of a multi-event input was sent
// and then the agent lost control, so the rest was not sent. Like
// errReadBackNoDriver it does not wrap ErrHandedOff, which means nothing was sent.
var errDriverLostMidAction = errors.New("control left the agent partway through the input; the rest was not sent")

// midActionFence rechecks the live driver between the events of one input.
func (r *actionRun) midActionFence() error {
	if err := r.s.driverFence(r.p); err != nil {
		return fmt.Errorf("%w (%v)", errDriverLostMidAction, err)
	}
	return nil
}

// errReadBackNoDriver is the cause when input was sent but the page could not
// be read back because the agent no longer drives the session. It deliberately
// does not wrap ErrHandedOff: that error means nothing was sent.
var errReadBackNoDriver = errors.New("input was sent, but control left the agent before the outcome could be read")

// clickSequence sends move, press and release over the action's connection and
// records the press so a timeout between press and release can be released.
func (r *actionRun) clickSequence(x, y float64) error {
	if _, err := r.conn.Call("Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved", "x": x, "y": y, "button": "none",
	}); err != nil {
		return err
	}
	// Each event is awaited, so a human can take control between them. The
	// driver is rechecked before the press and before the release; once the
	// move went out, a lost driver is an incomplete action, not a refusal.
	if err := r.midActionFence(); err != nil {
		return err
	}
	r.pressed, r.px, r.py = true, x, y
	if _, err := r.conn.Call("Input.dispatchMouseEvent", map[string]any{
		"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1,
	}); err != nil {
		return err
	}
	if err := r.midActionFence(); err != nil {
		return err
	}
	if _, err := r.conn.Call("Input.dispatchMouseEvent", map[string]any{
		"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1,
	}); err != nil {
		return err
	}
	r.pressed = false
	return nil
}

// statusWithURL is status() without the discovery probe: the caller already
// read the URL over its own connection.
func (s *cobrowseSession) statusWithURL(ctx context.Context, urls ...string) CobrowseSessionStatus {
	if s.tracking {
		// With page tracking the URL and the open pages come from the target
		// set, which has applied the page policy; a URL read from a page
		// directly has not been, so it is never reported or cached.
		return s.statusCtx(ctx)
	}
	st := s.statusNoProbe()
	for _, u := range urls {
		if u != "" {
			s.noteURL(u)
			st.URL = u
			return st
		}
	}
	st.URL = s.cachedURL()
	return st
}

// typedLength counts the characters sent, for the report.
func typedLength(text string) int { return utf8.RuneCountInString(text) }
