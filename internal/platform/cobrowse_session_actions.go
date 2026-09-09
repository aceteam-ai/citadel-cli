// internal/platform/cobrowse_session_actions.go
//
// Session-scoped CDP actions (issue #978): navigate / screenshot / click / type /
// extract, plus per-session driver arbitration (handoff / resume), addressed by
// session_id against the multi-session CobrowseSessionManager (cobrowse_session.go).
//
// This mirrors the singleton CobrowseManager's action set (cobrowse.go) rather than
// reinventing CDP interaction: navigate uses the same Page.navigate call, screenshot
// the same Page.captureScreenshot + base64 validation, and the driver-arbitration
// refusal reuses the exact same ErrHandedOff / ErrNotStarted sentinels and the same
// "refuse read-only actions too, not just writes" rule the singleton's Screenshot
// already enforces. click/type/extract have no singleton analog (the old COBROWSE job
// never grew them) and are new here, built on the same connect-act-disconnect
// cdpCommand/cdpDialAndSend plumbing (cobrowse.go) plus Runtime.evaluate for
// selector-based DOM interaction.
//
// Design choice, stated explicitly because the issue's own text guessed the opposite:
// screenshot and extract are READ-ONLY but are still refused while a human is
// attached, exactly like navigate/click/type. This mirrors the singleton's actual
// behavior (CobrowseManager.Screenshot refuses ErrHandedOff, not just Navigate) --
// not the issue text's "likely allowed during attach" guess, which turned out to be
// wrong once the singleton's code was read. A CDP round trip into a live page a human
// is driving is not a passive read from the human's point of view, so extract is held
// to the same rule for consistency.
//
// Driver arbitration is TWO independent bits, not one flag flipped by both halves of
// the interop pair:
//   - state == SessionAttached: a viewer is CURRENTLY connected (the #794 screencast
//     hook's MarkAttached/MarkDetached calls; server.go documents these as a
//     symmetric presence pair tied to the WebSocket connection's lifetime, and
//     cobrowsestream/handler.go calls them as `MarkAttached; defer MarkDetached`).
//     This bit is ephemeral: it clears itself the moment the viewer disconnects, with
//     NO change to cobrowse_session.go's setAttached (byte-identical to before #978).
//   - explicitHandoff: set by the `handoff` action, cleared by `resume`. This bit is
//     STICKY across a transient viewer disconnect -- the mid-2FA case: a network blip
//     dropping the viewer's WebSocket must not silently resume agent scripting on a
//     session the human explicitly claimed.
//
// humanDrivingLocked ORs them: an agent-scripted action is refused if EITHER a viewer
// is attached right now OR an explicit handoff is outstanding. This is why merely
// attaching (a passive watch-along) already satisfies "an agent write must be refused
// while a human is attached" and "a human can grab a live scripted session mid-run and
// hand it back" -- attach blocks, detach un-blocks -- with `handoff`/`resume` as the
// separate, sticky mechanism for a hold that must survive a reconnect. Collapsing
// these into one flag (making MarkAttached itself set a persistent "human driver" flag
// that only an explicit resume could clear) was tried and reverted: it would make ANY
// viewer connection -- including a passive watch-along with no intent to drive --
// permanently kill agent scripting until someone remembered to call `resume`, and the
// #8131/#8133 agents have no way to observe a viewer disconnect to know when that is
// safe.
package platform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
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
}

// humanDrivingLocked reports whether the session's driver-arbitration state
// currently blocks agent-scripted actions -- see the package doc comment above
// for why this is two bits ORed together rather than one flag. Caller holds s.mu.
func (s *cobrowseSession) humanDrivingLocked() bool {
	return s.explicitHandoff || s.state == SessionAttached
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
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no such browser session: %q", id)
	}
	return s, nil
}

// Navigate drives one session's browser to a URL. Refused while the session is
// handed off to a human (ErrHandedOff).
func (m *CobrowseSessionManager) Navigate(id, url string) (CobrowseSessionStatus, error) {
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
	return s.status(), nil
}

// Resume clears one session's sticky explicit-handoff bit. Idempotent. Note
// this does NOT by itself guarantee agent-scripted actions are allowed
// afterward: if a viewer is STILL attached (state == SessionAttached),
// humanDrivingLocked remains true on that bit alone and writes stay refused
// until the viewer disconnects too -- resume only releases an explicit claim,
// it does not evict a live viewer. Mirrors CobrowseManager.Resume.
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
	res, err := cdpCommand(debugPort, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  false,
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

// resolveSelectorCenter finds the first element matching selector, scrolls it
// into view, and returns the CSS-pixel viewport coordinates of its center. A
// selector matching nothing is a clear error, not a click at (0,0).
func resolveSelectorCenter(debugPort int, selector string) (float64, float64, error) {
	expr := fmt.Sprintf(`(function(){
		var el = document.querySelector(%s);
		if (!el) return null;
		el.scrollIntoView({block: "center", inline: "center"});
		var r = el.getBoundingClientRect();
		return {x: r.x + r.width / 2, y: r.y + r.height / 2};
	})()`, jsStringLiteral(selector))
	val, err := runtimeEvaluate(debugPort, expr)
	if err != nil {
		return 0, 0, err
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return 0, 0, fmt.Errorf("no element matches selector %q", selector)
	}
	x, _ := obj["x"].(float64)
	y, _ := obj["y"].(float64)
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
