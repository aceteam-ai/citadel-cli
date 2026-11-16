// Session-scoped CDP actions and driver arbitration for co-browse sessions.
// An attached viewer observes the browser without stopping agent actions.
// Explicit handoff gives the human control and blocks all agent CDP actions,
// including screenshot and extract. Resume returns control to the agent while
// the viewer remains attached. The handoff survives a viewer disconnect.
package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

// ExtractResult is the outcome of a session `extract` action: the matched
// element's text content plus any requested attribute values.
type ExtractResult struct {
	Text string `json:"text"`
	// Attrs holds only the attribute names the caller asked for (issue #978's
	// "extract (selector -> text/attrs)"), keyed by name. An attribute the
	// element does not have decodes as an empty string (matching
	// Element.getAttribute's null-to-caller contract), not a missing key --
	// so a caller can distinguish "asked for, absent" from "never asked for".
	Attrs map[string]string `json:"attrs,omitempty"`
	// Truncated is true when ExtractBounded cut Text at its character limit.
	Truncated bool `json:"truncated,omitempty"`
}

// ErrPasswordValue refuses an extract that asks for the value of a password
// field. Typed credentials must never travel back to the agent.
var ErrPasswordValue = fmt.Errorf("refusing to read the value of a password field")

// PageState is the current document's URL and readyState.
type PageState struct {
	URL        string `json:"url"`
	ReadyState string `json:"ready_state"`
	// Title is document.title, already bounded and stripped of control
	// characters (SanitizeCobrowseTitle).
	Title string `json:"title,omitempty"`
}

// MaxCobrowseTitleRunes bounds the page title returned from a navigation.
const MaxCobrowseTitleRunes = 512

// SanitizeCobrowseTitle drops control characters (including newlines and
// tabs, which a title has no use for), trims surrounding space and truncates
// to MaxCobrowseTitleRunes.
func SanitizeCobrowseTitle(t string) string {
	var b strings.Builder
	n := 0
	for _, r := range t {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if n >= MaxCobrowseTitleRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// Navigation settle tuning. The window is the most Navigate waits for the page
// to finish loading; it is deliberately below the 20s CDP command budget so a
// slow page returns whatever it has reached. Variables so tests need not sleep.
var (
	navigateSettleWindow  = 8 * time.Second
	navigateSettleMinimum = 400 * time.Millisecond
	navigateSettlePoll    = 100 * time.Millisecond
)

// settleNavigation polls the page until it reports readyState "complete" on a
// document that is not the pre-navigation one (or, when the URL did not change,
// after a short minimum so a pending navigation has time to commit), bounded by
// navigateSettleWindow. It returns the last state read; a slow page is not an
// error.
func settleNavigation(p pin, before string) PageState {
	return settlePage(p, before, false)
}

// settleLaunch is the start-up settle: the initial about:blank must not
// satisfy the minimum-wait shortcut, so it waits for the page to commit away
// from it (or for the settle window to end) and reports the current state.
func settleLaunch(p pin) PageState {
	return settlePage(p, "about:blank", true)
}

func settlePage(p pin, before string, requireChange bool) PageState {
	var last PageState
	start := time.Now()
	// One connection for the whole poll loop instead of a discovery and a
	// dial per tick (citadel-cli#1663). A failed read drops it and the next
	// tick reconnects.
	var conn *cdpConn
	var caller cdpCaller = p
	ctx, cancel := context.WithTimeout(context.Background(), navigateSettleWindow+cobrowseReportReserve)
	defer cancel()
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for {
		if conn == nil {
			if c, err := openCDPConn(ctx, p); err == nil {
				conn, caller = c, c
			}
		}
		ps, err := readPageStateOn(caller)
		if err != nil && conn != nil {
			conn.Close()
			conn, caller = nil, p
		}
		if err == nil {
			last = ps
			if ps.ReadyState == "" {
				// The page did not answer with a document state at all;
				// polling cannot improve on that.
				return ps
			}
			if ps.ReadyState == "complete" && (ps.URL != before || (!requireChange && time.Since(start) >= navigateSettleMinimum)) {
				return ps
			}
		}
		if time.Since(start) >= navigateSettleWindow {
			return last
		}
		time.Sleep(navigateSettlePoll)
	}
}

func readPageState(p pin) (PageState, error) { return readPageStateOn(p) }

func readPageStateOn(c cdpCaller) (PageState, error) {
	val, err := evaluateOn(c, `({url: location.href, ready: document.readyState, title: document.title})`, false)
	if err != nil {
		return PageState{}, err
	}
	obj, _ := val.(map[string]any)
	u, _ := obj["url"].(string)
	r, _ := obj["ready"].(string)
	t, _ := obj["title"].(string)
	return PageState{URL: u, ReadyState: r, Title: SanitizeCobrowseTitle(t)}, nil
}

type SelectResult struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// humanDrivingLocked reports whether a human explicitly took control. A viewer
// may observe a live session without blocking the agent. Caller holds s.mu.
func (s *cobrowseSession) humanDrivingLocked() bool {
	return s.explicitHandoff
}

// ViewerHasControl gates input arriving over the stream. The check is repeated
// for every input message so handback takes effect with the viewer still open.
func (m *CobrowseSessionManager) ViewerHasControl(id string) bool {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == SessionAttached && s.explicitHandoff
}

// driverFor projects humanDrivingLocked's bool onto the reported CobrowseDriver
// value.
func driverFor(humanDriving bool) CobrowseDriver {
	if humanDriving {
		return DriverHuman
	}
	return DriverAI
}

// requireDrivablePort returns the session's live CDP debug port for a scripted
// action (navigate, click, type, screenshot, extract), refusing when the browser
// is not running or has exited (ErrNotStarted) or is currently driven by a human
// (ErrHandedOff, per humanDrivingLocked). Every scripted CDP action funnels
// through this single check -- see the package doc comment above for why
// screenshot/extract are included, not just the mutating actions.
func (s *cobrowseSession) requireDrivablePort() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return 0, ErrNotStarted
	}
	select {
	case <-s.proc.exited:
		return 0, ErrNotStarted
	default:
	}
	if s.requireEgress && (s.egress == nil || !s.egress.Active()) {
		return 0, ErrEgressGuardUnavailable
	}
	if s.humanDrivingLocked() {
		return 0, ErrHandedOff
	}
	return s.proc.debugPort, nil
}

// pin is the one page a single action works on. Without page tracking
// (everything but local mode) it is the first page, exactly as before. With
// tracking, the active page is resolved once and every CDP command of the action
// goes to that page's own debugger socket; verify refuses input if the active
// page changed after it was chosen, so a popup that opens in between never
// receives a click or keystrokes meant for the page that was checked.
type pin struct {
	port int
	id   string
	ws   string
	set  *targetSet
	// refused: the page is the refused last page, pinned only to navigate away.
	refused bool
}

func (p pin) cdp(method string, params map[string]any) (map[string]any, error) {
	if p.ws == "" {
		return cdpCommand(p.port, method, params)
	}
	return cdpDialAndSend(p.ws, method, params)
}

// Call makes pin a cdpCaller that connects per call; budgeted actions use
// openCDPConn(p) instead to share one connection.
func (p pin) Call(method string, params map[string]any) (map[string]any, error) {
	return p.cdp(method, params)
}

// verify re-reads the pages and refuses when the pinned page is no longer the
// active one.
func (p pin) verify() error {
	return p.verifyCtx(context.Background())
}

// verifyCtx is verify under the caller's context, so an action's one budget
// covers it.
func (p pin) verifyCtx(parent context.Context) error {
	if p.set == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, targetSyncTimeout)
	defer cancel()
	snap, err := p.set.sync(ctx, p.port)
	if err != nil {
		return err
	}
	if snap.Active != p.id {
		return ErrTargetChanged
	}
	return nil
}

func pinFor(port int) (pin, error) { return pinForCtx(context.Background(), port) }

func pinForCtx(parent context.Context, port int) (pin, error) {
	set := targetSetFor(port)
	ctx, cancel := context.WithTimeout(parent, targetSyncTimeout)
	defer cancel()
	tg, err := set.activeTarget(ctx, port)
	if err != nil {
		return pin{}, err
	}
	return pin{port: port, id: tg.ID, ws: tg.WebSocketDebuggerURL, set: set}, nil
}

// pinNavigation is pinAction for a navigation: when the only page is refused by
// the policy, a navigation away from it is still allowed (it sends no input to
// the page), so the session can recover. Every other action stays refused.
func (s *cobrowseSession) pinNavigation() (pin, error) {
	p, err := s.pinAction()
	if !errors.Is(err, ErrPageRefused) {
		return p, err
	}
	port, perr := s.requireDrivablePort()
	if perr != nil {
		return pin{}, perr
	}
	set := targetSetFor(port)
	tg, ok := set.refusedTarget()
	if !ok {
		return pin{}, err
	}
	// The page the caller's checks covered must be the page we navigate.
	s.mu.Lock()
	expect := s.expectTarget
	s.mu.Unlock()
	if expect != "" && expect != tg.ID {
		return pin{}, ErrTargetChanged
	}
	return pin{port: port, id: tg.ID, ws: tg.WebSocketDebuggerURL, set: set, refused: true}, nil
}

// refusedRecoveryWait bounds how long a navigation away from a refused page
// waits for the policy to admit the new page; variables so tests need not sleep.
var (
	refusedRecoveryWait = 8 * time.Second
	refusedRecoveryPoll = 150 * time.Millisecond
)

// waitAllowedPage polls the page list (no evaluation) until the policy admits
// an active page, and returns a pin on it. If the page stays refused it fails
// with ErrPageRefused without ever reading the page.
func waitAllowedPage(p pin) (pin, error) {
	deadline := time.Now().Add(refusedRecoveryWait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), targetSyncTimeout)
		snap, err := p.set.sync(ctx, p.port)
		cancel()
		if err == nil && snap.Active != "" && snap.ActiveWebSocketURL != "" {
			return pin{port: p.port, id: snap.Active, ws: snap.ActiveWebSocketURL, set: p.set}, nil
		}
		if time.Now().After(deadline) {
			return pin{}, fmt.Errorf("%w (the navigation did not reach an allowed page)", ErrPageRefused)
		}
		time.Sleep(refusedRecoveryPoll)
	}
}

// pinAction checks the session is drivable and pins the page the action uses.
// When a caller recorded the page it checked (ExpectTarget), a different active
// page refuses the action.
func (s *cobrowseSession) pinAction() (pin, error) {
	return s.pinActionCtx(context.Background())
}

// pinActionCtx is pinAction under the caller's context: a budgeted action
// creates its context first so pinning spends the same budget.
func (s *cobrowseSession) pinActionCtx(ctx context.Context) (pin, error) {
	port, err := s.requireDrivablePort()
	if err != nil {
		return pin{}, err
	}
	s.mu.Lock()
	tracking, expect := s.tracking, s.expectTarget
	s.mu.Unlock()
	if !tracking {
		return pin{port: port}, nil
	}
	p, err := pinForCtx(ctx, port)
	if err != nil {
		return pin{}, err
	}
	if expect != "" && p.id != expect {
		return pin{}, ErrTargetChanged
	}
	return p, nil
}

// sessionForAction resolves a session by id for an action call, mirroring the
// "no such browser session" error the job handler already surfaces for status.
func (m *CobrowseSessionManager) sessionForAction(id string) (*cobrowseSession, error) {
	m.Recover()
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		if e, ended := m.EndedSession(id); ended {
			return nil, &SessionEndedError{ID: id, Reason: e.Reason, Detail: e.Detail}
		}
		return nil, fmt.Errorf("no such browser session: %q", id)
	}
	s.mu.Lock()
	missing := s.requireEgress && (s.egress == nil || !s.egress.Active())
	s.mu.Unlock()
	if missing {
		return nil, ErrEgressGuardUnavailable
	}
	return s, nil
}

// Navigate drives one session's browser to a URL. Refused while the session is
// handed off to a human (ErrHandedOff).
func (m *CobrowseSessionManager) Navigate(id, url string) (CobrowseSessionStatus, error) {
	if err := ValidateCobrowseURL(url); err != nil {
		return CobrowseSessionStatus{}, err
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	p, err := s.pinNavigation()
	if err != nil {
		// No status probe on a refused/not-started call: the job handler
		// discards the status on this path anyway, and status() does a real
		// CDP HTTP round trip (bounded, but still needless work on every
		// refused write).
		return CobrowseSessionStatus{}, err
	}
	if err := s.checkEgressURL(url); err != nil {
		return CobrowseSessionStatus{}, err
	}
	var before string
	if !p.refused { // nothing runs in a refused page but the navigation away
		if ps, err := readPageState(p); err == nil {
			before = ps.URL
		}
	}
	if _, err := p.cdp("Page.navigate", map[string]any{"url": url}); err != nil {
		return CobrowseSessionStatus{}, err
	}
	if p.refused {
		// Nothing is read from the refused page. Wait for the browser to report
		// an allowed page, then report that one; if it stays refused, fail.
		np, err := waitAllowedPage(p)
		if err != nil {
			return CobrowseSessionStatus{}, err
		}
		p = np
		before = ""
	}
	// Wait for the navigation (including redirects) to settle, then report
	// where the page ended up. A slow page returns its current state.
	ps := settleNavigation(p, before)
	st := s.statusWithURL(context.Background(), ps.URL)
	if ps.URL != "" {
		st.URL = ps.URL
	}
	st.Title = ps.Title
	return st, nil
}

// NavigateAsHuman navigates on behalf of the human who has taken control (the
// local viewer's address bar). It is the one navigation allowed while handed
// off, and it requires the handoff: the explicit-handoff bit stays set, so the
// agent stays blocked throughout.
func (m *CobrowseSessionManager) NavigateAsHuman(id, url string) (CobrowseSessionStatus, error) {
	if err := ValidateCobrowseURL(url); err != nil {
		return CobrowseSessionStatus{}, err
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	s.mu.Lock()
	if s.proc == nil {
		s.mu.Unlock()
		return CobrowseSessionStatus{}, ErrNotStarted
	}
	select {
	case <-s.proc.exited:
		s.mu.Unlock()
		return CobrowseSessionStatus{}, ErrNotStarted
	default:
	}
	if !s.humanDrivingLocked() {
		s.mu.Unlock()
		return CobrowseSessionStatus{}, fmt.Errorf("the user has not taken control of this session")
	}
	port := s.proc.debugPort
	tracking := s.tracking
	s.mu.Unlock()
	if err := s.checkEgressURL(url); err != nil {
		return CobrowseSessionStatus{}, err
	}
	p := pin{port: port}
	if tracking {
		if p, err = pinFor(port); errors.Is(err, ErrPageRefused) {
			// Navigating away from a refused page is the way out of it.
			tg, ok := targetSetFor(port).refusedTarget()
			if !ok {
				return CobrowseSessionStatus{}, err
			}
			p, err = pin{port: port, id: tg.ID, ws: tg.WebSocketDebuggerURL, set: targetSetFor(port), refused: true}, nil
		}
		if err != nil {
			return CobrowseSessionStatus{}, err
		}
	}
	if _, err := p.cdp("Page.navigate", map[string]any{"url": url}); err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.status(), nil
}

// Screenshot captures one session's current viewport as base64 PNG. Refused
// while handed off to a human -- mirrors CobrowseManager.Screenshot exactly
// (see the package doc comment above).
func (m *CobrowseSessionManager) Screenshot(id string) (string, error) {
	data, _, err := m.ScreenshotStatus(id)
	return data, err
}

// ScreenshotStatus is Screenshot plus the session status, read over the same
// connection to the pinned page: one dial instead of one per call, and no
// second probe for the URL (citadel-cli#1663).
func (m *CobrowseSessionManager) ScreenshotStatus(id string) (string, CobrowseSessionStatus, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return "", CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	ctx := b.act
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return "", CobrowseSessionStatus{}, err
	}
	conn, err := openCDPConn(ctx, p)
	if err != nil {
		return "", CobrowseSessionStatus{}, err
	}
	defer conn.Close()
	// Final fence: nothing awaited after this precedes the capture.
	if err := s.fence(ctx, p); err != nil {
		return "", CobrowseSessionStatus{}, err
	}
	// optimizeForSpeed trades PNG size for encode time, which dominates on a
	// software-rendered display.
	res, err := conn.Call("Page.captureScreenshot", map[string]any{"format": "png", "optimizeForSpeed": true})
	if err != nil {
		return "", CobrowseSessionStatus{}, err
	}
	data, _ := res["data"].(string)
	if data == "" {
		return "", CobrowseSessionStatus{}, fmt.Errorf("empty screenshot data from CDP")
	}
	if _, derr := base64.StdEncoding.DecodeString(data); derr != nil {
		return "", CobrowseSessionStatus{}, fmt.Errorf("screenshot not valid base64: %w", derr)
	}
	// Tracked sessions take the URL from the target set (statusWithURL); an
	// untracked one reads it from the discovery that found the page.
	return data, s.statusWithURL(b.op, conn.target.URL), nil
}

// Click performs a scripted mouse click in one session's browser: either at the
// center of the first element matching selector, or at an explicit CSS-pixel
// viewport coordinate when x and y are both non-nil. Exactly one form must be
// supplied; callers (the job handler) validate the payload shape before calling
// in, but this also refuses a call that supplies neither, so a direct caller
// gets the same clear error rather than a confusing CDP failure.
func (m *CobrowseSessionManager) Click(id, selector string, x, y *float64) (CobrowseSessionStatus, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	if selector == "" && (x == nil || y == nil) {
		return CobrowseSessionStatus{}, fmt.Errorf("click requires a 'selector' or both 'x' and 'y'")
	}
	return s.runAction(b, p, "click", func(r *actionRun) error {
		var cx, cy float64
		if selector != "" {
			var err error
			cx, cy, err = resolveSelectorCenterOn(r.conn, selector)
			if err != nil {
				return err
			}
		} else {
			cx, cy = *x, *y
		}
		// Selector settling awaits page JavaScript, during which a human can
		// take control or the active page can change. The final fence checks
		// the live driver and the pinned page after the last await and before
		// any mouse event.
		if err := r.fence(); err != nil {
			return err
		}
		r.eff.Dispatched = true
		if err := r.clickSequence(cx, cy); err != nil {
			return err
		}
		r.eff.Clicked = true
		return nil
	})
}

// Type inserts text into whatever element currently holds focus in one
// session's browser (via CDP Input.insertText) -- the caller is expected to
// have focused a field first, typically with a preceding `click`.
func (m *CobrowseSessionManager) Type(id, text string) (CobrowseSessionStatus, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.runAction(b, p, "type", func(r *actionRun) error {
		if err := r.fence(); err != nil {
			return err
		}
		r.eff.Dispatched = true
		r.eff.TypedLength, r.eff.TypedUnconfirmed = typedLength(text), true
		if _, err := r.conn.Call("Input.insertText", map[string]any{"text": text}); err != nil {
			return err
		}
		r.eff.TypedUnconfirmed = false
		return nil
	})
}

// Select changes a native select through the page DOM. Native popup windows do
// not receive forwarded CDP mouse clicks, so mouse coordinates cannot choose
// an option reliably. The native setter and bubbling events update React forms.
func (m *CobrowseSessionManager) Select(id, selector, value, label string) (SelectResult, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return SelectResult{}, err
	}
	p, err := s.pinAction()
	if err != nil {
		return SelectResult{}, err
	}
	expr := fmt.Sprintf(`(function(){
		const el = document.querySelector(%s);
		if (!(el instanceof HTMLSelectElement)) return {error: "selector does not match a select"};
		const options = Array.from(el.options);
		const option = options.find(o => %s !== "" ? o.value === %s : o.label === %s);
		if (el.matches(":disabled") || !option || option.disabled || option.parentElement?.disabled) return {error: "enabled option not found"};
		const setter = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, "value").set;
		setter.call(el, option.value);
		el.dispatchEvent(new Event("input", {bubbles: true}));
		el.dispatchEvent(new Event("change", {bubbles: true}));
		return {value: el.value, label: el.selectedOptions[0]?.label || ""};
	})()`, jsStringLiteral(selector), jsStringLiteral(value), jsStringLiteral(value), jsStringLiteral(label))
	result, err := runtimeEvaluate(p, expr)
	if err != nil {
		return SelectResult{}, err
	}
	obj, ok := result.(map[string]any)
	if !ok {
		return SelectResult{}, fmt.Errorf("select returned no result")
	}
	if problem, _ := obj["error"].(string); problem != "" {
		return SelectResult{}, fmt.Errorf("select: %s", problem)
	}
	selected, ok := obj["value"].(string)
	if !ok {
		return SelectResult{}, fmt.Errorf("select returned no value")
	}
	selectedLabel, _ := obj["label"].(string)
	return SelectResult{Value: selected, Label: selectedLabel}, nil
}

// Key sends a real key press and release. Text is never logged by the job
// handler; the MCP audit records only the action name.
func (m *CobrowseSessionManager) Key(id, key string) (CobrowseSessionStatus, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	code, vk, produced, ok := browserKey(key)
	if !ok {
		return CobrowseSessionStatus{}, fmt.Errorf("unsupported key")
	}
	kind := "rawKeyDown"
	if produced != "" {
		kind = "keyDown"
	}
	params := map[string]any{"type": kind, "key": key, "code": code, "windowsVirtualKeyCode": vk}
	if produced != "" {
		params["text"] = produced
	}
	return s.runAction(b, p, "key", func(r *actionRun) error {
		if err := r.fence(); err != nil {
			return err
		}
		r.eff.Dispatched = true
		if _, err := r.conn.Call("Input.dispatchKeyEvent", params); err != nil {
			return err
		}
		// A human can take control while the key-down is awaited.
		if err := r.midActionFence(); err != nil {
			return err
		}
		_, err := r.conn.Call("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": code, "windowsVirtualKeyCode": vk})
		return err
	})
}

func browserKey(key string) (string, int, string, bool) {
	special := map[string]struct {
		code string
		vk   int
	}{
		"Tab": {"Tab", 9}, "Enter": {"Enter", 13}, "Escape": {"Escape", 27},
		"ArrowUp": {"ArrowUp", 38}, "ArrowDown": {"ArrowDown", 40},
		"ArrowLeft": {"ArrowLeft", 37}, "ArrowRight": {"ArrowRight", 39},
		"Home": {"Home", 36}, "End": {"End", 35}, "PageUp": {"PageUp", 33},
		"PageDown": {"PageDown", 34}, "Backspace": {"Backspace", 8}, "Delete": {"Delete", 46},
	}
	if s, ok := special[key]; ok {
		return s.code, s.vk, "", true
	}
	if len(key) == 1 {
		ch := key[0]
		if ch >= 'a' && ch <= 'z' {
			return "Key" + strings.ToUpper(key), int(ch - 'a' + 'A'), key, true
		}
		if ch >= 'A' && ch <= 'Z' {
			return "Key" + key, int(ch), key, true
		}
		if ch >= '0' && ch <= '9' {
			return "Digit" + key, int(ch), key, true
		}
		if ch == ' ' {
			return "Space", 32, key, true
		}
	}
	return "", 0, "", false
}

func (m *CobrowseSessionManager) Scroll(id, selector string, x, y *float64, dx, dy float64) (CobrowseSessionStatus, error) {
	if math.IsNaN(dx) || math.IsInf(dx, 0) || math.IsNaN(dy) || math.IsInf(dy, 0) {
		return CobrowseSessionStatus{}, fmt.Errorf("scroll deltas must be finite")
	}
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	b, cancel := newActionBudget()
	defer cancel()
	p, err := s.pinActionCtx(b.op)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	if selector == "" && (x == nil || y == nil) {
		return CobrowseSessionStatus{}, fmt.Errorf("scroll requires selector or x and y")
	}
	return s.runAction(b, p, "scroll", func(r *actionRun) error {
		var cx, cy float64
		if selector != "" {
			var err error
			cx, cy, err = resolveSelectorCenterOn(r.conn, selector)
			if err != nil {
				return err
			}
		} else {
			cx, cy = *x, *y
		}
		if err := r.fence(); err != nil {
			return err
		}
		r.eff.Dispatched = true
		_, err := r.conn.Call("Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": cx, "y": cy, "deltaX": dx, "deltaY": dy})
		return err
	})
}

// Extract reads the text content (and, optionally, named attribute values) of
// the first element matching selector in one session's browser.
func (m *CobrowseSessionManager) Extract(id, selector string, attrs []string) (ExtractResult, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return ExtractResult{}, err
	}
	p, err := s.pinAction()
	if err != nil {
		return ExtractResult{}, err
	}
	return extractElement(p, selector, attrs)
}

// ExtractBounded is Extract with the text cut to maxChars inside the page (so an
// oversized document never crosses CDP whole) and Truncated reported. It refuses
// to return a "value" attribute from a password input. maxChars <= 0 means no
// limit.
func (m *CobrowseSessionManager) ExtractBounded(id, selector string, attrs []string, maxChars int) (ExtractResult, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return ExtractResult{}, err
	}
	p, err := s.pinAction()
	if err != nil {
		return ExtractResult{}, err
	}
	if attrs == nil {
		attrs = []string{}
	}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return ExtractResult{}, err
	}
	expr := fmt.Sprintf(`(function(){
		var el = document.querySelector(%s);
		if (!el) return null;
		var names = %s;
		var isPassword = (el instanceof HTMLInputElement) && String(el.type).toLowerCase() === "password";
		for (var i = 0; i < names.length; i++) {
			if (isPassword && String(names[i]).toLowerCase() === "value") return {password: true};
		}
		var attrs = {};
		for (var j = 0; j < names.length; j++) {
			attrs[names[j]] = el.getAttribute(names[j]) || "";
		}
		var text = (el.innerText !== undefined ? el.innerText : el.textContent) || "";
		var max = %d;
		var truncated = false;
		if (max > 0 && text.length > max) { text = text.slice(0, max); truncated = true; }
		return {text: text, attrs: attrs, truncated: truncated};
	})()`, jsStringLiteral(selector), string(attrsJSON), maxChars)
	val, err := runtimeEvaluate(p, expr)
	if err != nil {
		return ExtractResult{}, err
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return ExtractResult{}, fmt.Errorf("no element matches selector %q", selector)
	}
	if pw, _ := obj["password"].(bool); pw {
		return ExtractResult{}, ErrPasswordValue
	}
	text, _ := obj["text"].(string)
	truncated, _ := obj["truncated"].(bool)
	result := ExtractResult{Text: text, Truncated: truncated}
	if rawAttrs, ok := obj["attrs"].(map[string]any); ok && len(rawAttrs) > 0 {
		result.Attrs = make(map[string]string, len(rawAttrs))
		for k, v := range rawAttrs {
			if sv, ok := v.(string); ok {
				result.Attrs[k] = sv
			}
		}
	}
	return result, nil
}

// PageState reads the session's current URL and document.readyState. It is an
// agent read, so it is refused while a human drives, like screenshot.
func (m *CobrowseSessionManager) PageState(id string) (PageState, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return PageState{}, err
	}
	p, err := s.pinAction()
	if err != nil {
		return PageState{}, err
	}
	return readPageState(p)
}

// Handoff sets the STICKY explicit-handoff bit on one session (see the package
// doc comment's two-bit explanation), refusing subsequent agent-scripted
// actions even across a transient viewer disconnect -- the mid-2FA case.
// Idempotent (calling it while already handed off is a no-op success),
// mirroring CobrowseManager.Handoff.
func (m *CobrowseSessionManager) Handoff(id string) (CobrowseSessionStatus, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	s.mu.Lock()
	if s.proc == nil {
		s.mu.Unlock()
		return CobrowseSessionStatus{}, ErrNotStarted
	}
	s.explicitHandoff = true
	s.mu.Unlock()
	s.persistRecord()
	return s.status(), nil
}

// Resume clears the explicit handoff and restores agent actions even when a
// viewer remains attached. The viewer can continue observing. Idempotent.
func (m *CobrowseSessionManager) Resume(id string) (CobrowseSessionStatus, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	s.mu.Lock()
	if s.proc == nil {
		s.mu.Unlock()
		return CobrowseSessionStatus{}, ErrNotStarted
	}
	s.explicitHandoff = false
	s.mu.Unlock()
	s.persistRecord()
	return s.status(), nil
}

// ---------------------------------------------------------------------------
// CDP scripting helpers (Runtime.evaluate + Input.dispatch*). Reuses
// cdpCommand/cdpDialAndSend from cobrowse.go (connect-act-disconnect, same
// package) rather than a second CDP client implementation.
// ---------------------------------------------------------------------------

// jsStringLiteral safely embeds an arbitrary Go string as a JS string literal:
// a JSON string literal is always a valid JS string literal, so this is a
// correct (and simple) escape with no injection risk from the selector text.
func jsStringLiteral(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// runtimeEvaluate runs expr as a CDP Runtime.evaluate call with
// returnByValue=true and decodes the JSON-serializable result value. Returns
// an error if the script itself threw (CDP's exceptionDetails).
func runtimeEvaluate(p pin, expr string) (any, error) {
	return runtimeEvaluateWithPromise(p, expr, false)
}

func runtimeEvaluateWithPromise(p pin, expr string, awaitPromise bool) (any, error) {
	return evaluateOn(p, expr, awaitPromise)
}

// resolveSelectorCenter waits for the first matching element to stop moving
// after scrolling and verifies that the click point still hits that element.
func resolveSelectorCenter(p pin, selector string) (float64, float64, error) {
	return resolveSelectorCenterOn(p, selector)
}

func resolveSelectorCenterOn(c cdpCaller, selector string) (float64, float64, error) {
	expr := fmt.Sprintf(`(async function(){
		var el = document.querySelector(%s);
		if (!el) return null;
		el.scrollIntoView({block: "center", inline: "center"});
		var deadline = Date.now() + 2500;
		var previous = null;
		var stable = 0;
		while (Date.now() < deadline) {
			if (!el.isConnected) return {error: "element was removed while scrolling"};
			var r = el.getBoundingClientRect();
			var x = r.x + r.width / 2;
			var y = r.y + r.height / 2;
			var visible = r.width > 0 && r.height > 0 &&
				x >= 0 && x < window.innerWidth && y >= 0 && y < window.innerHeight;
			if (visible && previous && Math.abs(x - previous.x) < 0.5 &&
				Math.abs(y - previous.y) < 0.5) stable++;
			else stable = 0;
			if (stable >= 2) {
				var hit = document.elementFromPoint(x, y);
				if (!hit || (hit !== el && !el.contains(hit)))
					return {error: "another element covers the click point"};
				return {x: x, y: y};
			}
			previous = {x: x, y: y};
			await new Promise(function(resolve) { setTimeout(resolve, 50); });
		}
		return {error: "element did not settle in the viewport"};
	})()`, jsStringLiteral(selector))
	val, err := evaluateOn(c, expr, true)
	if err != nil {
		return 0, 0, err
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return 0, 0, fmt.Errorf("no element matches selector %q", selector)
	}
	if reason, ok := obj["error"].(string); ok {
		return 0, 0, fmt.Errorf("cannot click selector %q: %s", selector, reason)
	}
	x, xOK := obj["x"].(float64)
	y, yOK := obj["y"].(float64)
	if !xOK || !yOK {
		return 0, 0, fmt.Errorf("invalid click point for selector %q", selector)
	}
	return x, y, nil
}

// clickAtPoint dispatches a synthetic left click (move, press, release) at the
// given CSS-pixel viewport coordinates.
func clickAtPoint(p pin, x, y float64) error {
	if err := p.verify(); err != nil {
		return err
	}
	return clickAtPointOn(p, x, y)
}

func clickAtPointOn(c cdpCaller, x, y float64) error {
	if _, err := c.Call("Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved", "x": x, "y": y, "button": "none",
	}); err != nil {
		return err
	}
	if _, err := c.Call("Input.dispatchMouseEvent", map[string]any{
		"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1,
	}); err != nil {
		return err
	}
	_, err := c.Call("Input.dispatchMouseEvent", map[string]any{
		"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1,
	})
	return err
}

// extractElement reads the first element matching selector's text content and
// the requested attribute values in one Runtime.evaluate round trip.
func extractElement(p pin, selector string, attrNames []string) (ExtractResult, error) {
	if attrNames == nil {
		attrNames = []string{}
	}
	attrsJSON, err := json.Marshal(attrNames)
	if err != nil {
		return ExtractResult{}, err
	}
	expr := fmt.Sprintf(`(function(){
		var el = document.querySelector(%s);
		if (!el) return null;
		var names = %s;
		var attrs = {};
		for (var i = 0; i < names.length; i++) {
			attrs[names[i]] = el.getAttribute(names[i]) || "";
		}
		var text = (el.innerText !== undefined ? el.innerText : el.textContent) || "";
		return {text: text, attrs: attrs};
	})()`, jsStringLiteral(selector), string(attrsJSON))

	val, err := runtimeEvaluate(p, expr)
	if err != nil {
		return ExtractResult{}, err
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return ExtractResult{}, fmt.Errorf("no element matches selector %q", selector)
	}
	text, _ := obj["text"].(string)
	result := ExtractResult{Text: text}
	if rawAttrs, ok := obj["attrs"].(map[string]any); ok && len(rawAttrs) > 0 {
		result.Attrs = make(map[string]string, len(rawAttrs))
		for k, v := range rawAttrs {
			if sv, ok := v.(string); ok {
				result.Attrs[k] = sv
			}
		}
	}
	return result, nil
}
