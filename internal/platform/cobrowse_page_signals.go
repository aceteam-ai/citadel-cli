package platform

import (
	"context"
	"strings"
	"unicode"
)

// MaxCobrowsePageTextRunes bounds the page_text a start or status result
// carries, counted after whitespace collapse.
const MaxCobrowsePageTextRunes = 2048

// pageSignalsScript runs in an isolated world of the main frame. It reads the
// visible text (cut to 32768 UTF-16 units in the page, so a huge body is never
// shipped over CDP) and whether a visible password input exists, nothing else.
const pageSignalsScript = `(function(){
	var b = document.body;
	var t = b ? String(b.innerText || "") : "";
	var pw = false;
	var els = document.querySelectorAll('input[type=password]');
	for (var i = 0; i < els.length && !pw; i++) {
		var r = els[i].getBoundingClientRect();
		var cs = getComputedStyle(els[i]);
		if (r.width > 0 && r.height > 0 && cs.visibility !== "hidden" && cs.display !== "none") pw = true;
	}
	return {t: t.slice(0, 32768), p: pw};
})()`

// PageSignals are the two content signals the platform uses to tell a login
// wall served in place from an authenticated page.
type PageSignals struct {
	Text          string
	PasswordField bool
}

// pageSignalsGate, pageSignalsConnect and pageSignalsPin are seams for tests.
var (
	pageSignalsPin     = (*cobrowseSession).pinActionCtx
	pageSignalsGate    = pageActiveErr
	pageSignalsConnect = func(ctx context.Context, p pin) (cdpCaller, func(), error) {
		c, err := openCDPConn(ctx, p)
		if err != nil {
			return nil, nil, err
		}
		return c, c.Close, nil
	}
)

// CollapsePageText collapses whitespace runs to one space, drops other control
// characters, trims, and truncates to MaxCobrowsePageTextRunes.
func CollapsePageText(raw string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range raw {
		if unicode.IsSpace(r) || r == ' ' || r == ' ' {
			space = b.Len() > 0
			continue
		}
		// Cc (controls) and Cf (bidi marks, zero-width characters).
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if space {
			if n+1 >= MaxCobrowsePageTextRunes {
				break
			}
			b.WriteByte(' ')
			n++
			space = false
		}
		if n >= MaxCobrowsePageTextRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// readPageSignalsOn reads the signals through c in an isolated world of the
// main frame. Any failure reports false: the caller omits the fields.
//
// fence is the live driver check. It runs after every await (the frame tree,
// the isolated world, and the evaluate), so a handoff at any point omits both
// fields; the evaluate itself is never sent once the fence has refused.
func readPageSignalsOn(c cdpCaller, fence func() error) (PageSignals, bool) {
	if fence == nil {
		fence = func() error { return nil }
	}
	tree, err := c.Call("Page.getFrameTree", nil)
	if err != nil || fence() != nil {
		return PageSignals{}, false
	}
	frameID := rootFrameID(tree)
	if frameID == "" {
		return PageSignals{}, false
	}
	w, err := c.Call("Page.createIsolatedWorld", map[string]any{"frameId": frameID, "worldName": "citadel-signals"})
	if err != nil || fence() != nil {
		return PageSignals{}, false
	}
	ctxID, ok := w["executionContextId"].(float64)
	if !ok || ctxID == 0 {
		return PageSignals{}, false
	}
	res, err := c.Call("Runtime.evaluate", map[string]any{
		"expression":    pageSignalsScript,
		"contextId":     int(ctxID),
		"returnByValue": true,
	})
	if err != nil || fence() != nil {
		return PageSignals{}, false
	}
	if exc, ok := res["exceptionDetails"]; ok && exc != nil {
		return PageSignals{}, false
	}
	remote, _ := res["result"].(map[string]any)
	obj, _ := remote["value"].(map[string]any)
	if obj == nil {
		return PageSignals{}, false
	}
	t, _ := obj["t"].(string)
	p, _ := obj["p"].(bool)
	return PageSignals{Text: CollapsePageText(t), PasswordField: p}, true
}

// readPageSignals reads the signals of the pinned page, after the page policy
// gate and under ctx. It reports false when the page is refused or not the
// active one, or when anything fails.
func readPageSignals(ctx context.Context, p pin, fence func() error) (PageSignals, bool) {
	if err := pageSignalsGate(ctx, p); err != nil {
		return PageSignals{}, false
	}
	c, closeFn, err := pageSignalsConnect(ctx, p)
	if err != nil {
		return PageSignals{}, false
	}
	defer closeFn()
	if fence != nil && fence() != nil {
		return PageSignals{}, false
	}
	return readPageSignalsOn(c, fence)
}

// pageSignals reads the signals for a node-wide session. Page-tracking
// (local mode) sessions never report them. pinActionCtx refuses a session that
// a human drives (ErrHandedOff), so nothing is read then.
func (s *cobrowseSession) pageSignals(ctx context.Context) (PageSignals, bool) {
	s.mu.Lock()
	tracking := s.tracking
	s.mu.Unlock()
	if tracking {
		return PageSignals{}, false
	}
	p, err := pageSignalsPin(s, ctx)
	if err != nil {
		return PageSignals{}, false
	}
	return readPageSignals(ctx, p, func() error { return s.driverFence(p) })
}

// WithPageSignals adds page_text and password_field to a start or status
// result for a node-wide session (the owner of both fields, aceteam#13773).
// They are omitted whenever they cannot be read safely: a handed-off, refused,
// tracking, ended or still-loading session, or any failure. The one budget is
// the action report reserve. page_text is page content: it is never logged or
// put in the registry record.
func (m *CobrowseSessionManager) WithPageSignals(st CobrowseSessionStatus) CobrowseSessionStatus {
	st = withBrowserInterstitial(st)
	if st.Interstitial != "" || st.FirstLoad == FirstLoadPending || st.Driver == DriverHuman {
		return st
	}
	m.mu.Lock()
	s := m.sessions[st.ID]
	m.mu.Unlock()
	if s == nil {
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), cobrowseReportReserve)
	defer cancel()
	sig, ok := s.pageSignals(ctx)
	if !ok {
		return st
	}
	st.PageText = sig.Text
	pw := sig.PasswordField
	st.PasswordField = &pw
	return st
}
