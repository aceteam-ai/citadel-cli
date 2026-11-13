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
func settleNavigation(port int, before string) PageState {
	var last PageState
	start := time.Now()
	for {
		if ps, err := readPageState(port); err == nil {
			last = ps
			if ps.ReadyState == "" {
				// The page did not answer with a document state at all;
				// polling cannot improve on that.
				return ps
			}
			if ps.ReadyState == "complete" && (ps.URL != before || time.Since(start) >= navigateSettleMinimum) {
				return ps
			}
		}
		if time.Since(start) >= navigateSettleWindow {
			return last
		}
		time.Sleep(navigateSettlePoll)
	}
}

func readPageState(port int) (PageState, error) {
	val, err := runtimeEvaluate(port, `({url: location.href, ready: document.readyState, title: document.title})`)
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
	if s.humanDrivingLocked() {
		return 0, ErrHandedOff
	}
	return s.proc.debugPort, nil
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
	return s, nil
}

// WithRecordingPort gives one bounded recording the CDP port. Stop, handoff and
// viewer attachment cancel the capture and wait for it to discard its temporary
// output. The publish callback is serialized with those privacy transitions:
// once one starts, the recording cannot publish a file or return a receipt.
func (m *CobrowseSessionManager) WithRecordingPort(
	id string,
	parent context.Context,
	action func(context.Context, int, func(func() error) error) ([]byte, error),
) ([]byte, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.proc == nil {
		s.mu.Unlock()
		return nil, ErrNotStarted
	}
	select {
	case <-s.proc.exited:
		s.mu.Unlock()
		return nil, ErrNotStarted
	default:
	}
	if s.humanDrivingLocked() || s.state == SessionAttached {
		s.mu.Unlock()
		return nil, ErrHandedOff
	}
	if s.recordDone != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("a recording is already in progress")
	}
	if parent == nil {
		parent = context.Background()
	}
	recordCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.recordCancel = cancel
	s.recordDone = done
	port := s.proc.debugPort
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		if s.recordDone == done {
			s.recordCancel = nil
			s.recordDone = nil
			close(done)
		}
		s.mu.Unlock()
	}()

	committed := false
	commit := func(publish func() error) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.recordDone != done {
			return context.Canceled
		}
		if err := recordCtx.Err(); err != nil {
			return err
		}
		if s.proc == nil || s.humanDrivingLocked() || s.state == SessionAttached {
			return ErrHandedOff
		}
		if err := publish(); err != nil {
			return err
		}
		committed = true
		s.recordCancel = nil
		s.recordDone = nil
		close(done)
		return nil
	}
	result, err := action(recordCtx, port, commit)
	if err == nil && !committed {
		return nil, fmt.Errorf("recording did not publish an output")
	}
	return result, err
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
	port, err := s.requireDrivablePort()
	if err != nil {
		// No status probe on a refused/not-started call: the job handler
		// discards the status on this path anyway, and status() does a real
		// CDP HTTP round trip (bounded, but still needless work on every
		// refused write).
		return CobrowseSessionStatus{}, err
	}
	var before string
	if ps, err := readPageState(port); err == nil {
		before = ps.URL
	}
	if _, err := cdpCommand(port, "Page.navigate", map[string]any{"url": url}); err != nil {
		return CobrowseSessionStatus{}, err
	}
	// Wait for the navigation (including redirects) to settle, then report
	// where the page ended up. A slow page returns its current state.
	ps := settleNavigation(port, before)
	st := s.status()
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
	s.mu.Unlock()
	if _, err := cdpCommand(port, "Page.navigate", map[string]any{"url": url}); err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.status(), nil
}

// Screenshot captures one session's current viewport as base64 PNG. Refused
// while handed off to a human -- mirrors CobrowseManager.Screenshot exactly
// (see the package doc comment above).
func (m *CobrowseSessionManager) Screenshot(id string) (string, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return "", err
	}
	port, err := s.requireDrivablePort()
	if err != nil {
		return "", err
	}
	res, err := cdpCommand(port, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return "", err
	}
	data, _ := res["data"].(string)
	if data == "" {
		return "", fmt.Errorf("empty screenshot data from CDP")
	}
	if _, derr := base64.StdEncoding.DecodeString(data); derr != nil {
		return "", fmt.Errorf("screenshot not valid base64: %w", derr)
	}
	return data, nil
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
	port, err := s.requireDrivablePort()
	if err != nil {
		return CobrowseSessionStatus{}, err
	}

	var cx, cy float64
	switch {
	case selector != "":
		cx, cy, err = resolveSelectorCenter(port, selector)
		if err != nil {
			return CobrowseSessionStatus{}, err
		}
	case x != nil && y != nil:
		cx, cy = *x, *y
	default:
		return CobrowseSessionStatus{}, fmt.Errorf("click requires a 'selector' or both 'x' and 'y'")
	}

	// Selector settling awaits page JavaScript, during which a human can take
	// control. Check the live driver again before sending any mouse event.
	currentPort, err := s.requireDrivablePort()
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	if currentPort != port {
		return CobrowseSessionStatus{}, ErrNotStarted
	}
	if err := clickAtPoint(port, cx, cy); err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.status(), nil
}

// Type inserts text into whatever element currently holds focus in one
// session's browser (via CDP Input.insertText) -- the caller is expected to
// have focused a field first, typically with a preceding `click`.
func (m *CobrowseSessionManager) Type(id, text string) (CobrowseSessionStatus, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	port, err := s.requireDrivablePort()
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	if _, err := cdpCommand(port, "Input.insertText", map[string]any{"text": text}); err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.status(), nil
}

// Select changes a native select through the page DOM. Native popup windows do
// not receive forwarded CDP mouse clicks, so mouse coordinates cannot choose
// an option reliably. The native setter and bubbling events update React forms.
func (m *CobrowseSessionManager) Select(id, selector, value, label string) (SelectResult, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return SelectResult{}, err
	}
	port, err := s.requireDrivablePort()
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
	result, err := runtimeEvaluate(port, expr)
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
	port, err := s.requireDrivablePort()
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
	if _, err := cdpCommand(port, "Input.dispatchKeyEvent", params); err != nil {
		return CobrowseSessionStatus{}, err
	}
	if _, err := cdpCommand(port, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "code": code, "windowsVirtualKeyCode": vk}); err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.status(), nil
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
	port, err := s.requireDrivablePort()
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	var cx, cy float64
	if selector != "" {
		cx, cy, err = resolveSelectorCenter(port, selector)
	} else if x != nil && y != nil {
		cx, cy = *x, *y
	} else {
		return CobrowseSessionStatus{}, fmt.Errorf("scroll requires selector or x and y")
	}
	if err != nil {
		return CobrowseSessionStatus{}, err
	}
	if _, err := cdpCommand(port, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": cx, "y": cy, "deltaX": dx, "deltaY": dy}); err != nil {
		return CobrowseSessionStatus{}, err
	}
	return s.status(), nil
}

// Extract reads the text content (and, optionally, named attribute values) of
// the first element matching selector in one session's browser.
func (m *CobrowseSessionManager) Extract(id, selector string, attrs []string) (ExtractResult, error) {
	s, err := m.sessionForAction(id)
	if err != nil {
		return ExtractResult{}, err
	}
	port, err := s.requireDrivablePort()
	if err != nil {
		return ExtractResult{}, err
	}
	return extractElement(port, selector, attrs)
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
	port, err := s.requireDrivablePort()
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
	val, err := runtimeEvaluate(port, expr)
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
	port, err := s.requireDrivablePort()
	if err != nil {
		return PageState{}, err
	}
	return readPageState(port)
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
	done := s.cancelRecordingLocked()
	s.mu.Unlock()
	if done != nil {
		<-done
	}
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
func runtimeEvaluate(debugPort int, expr string) (any, error) {
	return runtimeEvaluateWithPromise(debugPort, expr, false)
}

func runtimeEvaluateWithPromise(debugPort int, expr string, awaitPromise bool) (any, error) {
	res, err := cdpCommand(debugPort, "Runtime.evaluate", map[string]any{
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

// resolveSelectorCenter waits for the first matching element to stop moving
// after scrolling and verifies that the click point still hits that element.
func resolveSelectorCenter(debugPort int, selector string) (float64, float64, error) {
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
	val, err := runtimeEvaluateWithPromise(debugPort, expr, true)
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
func clickAtPoint(debugPort int, x, y float64) error {
	if _, err := cdpCommand(debugPort, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseMoved", "x": x, "y": y, "button": "none",
	}); err != nil {
		return err
	}
	if _, err := cdpCommand(debugPort, "Input.dispatchMouseEvent", map[string]any{
		"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1,
	}); err != nil {
		return err
	}
	_, err := cdpCommand(debugPort, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1,
	})
	return err
}

// extractElement reads the first element matching selector's text content and
// the requested attribute values in one Runtime.evaluate round trip.
func extractElement(debugPort int, selector string, attrNames []string) (ExtractResult, error) {
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

	val, err := runtimeEvaluate(debugPort, expr)
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
