package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// signalsCaller is a fake CDP endpoint for the page-signals read.
type signalsCaller struct {
	text      string
	pw        bool
	failOn    string
	calls     []string
	evalParam map[string]any
}

func (f *signalsCaller) Call(method string, params map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, method)
	if method == f.failOn {
		return nil, errors.New("boom")
	}
	switch method {
	case "Page.getFrameTree":
		return frameTreeResult([]fakeFrame{{ID: "R", URL: "https://x.example/"}}), nil
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": float64(7)}, nil
	case "Runtime.evaluate":
		f.evalParam = params
		return map[string]any{"result": map[string]any{"value": map[string]any{"t": f.text, "p": f.pw}}}, nil
	}
	return map[string]any{}, nil
}

func TestPageSignalsUseIsolatedWorld(t *testing.T) {
	f := &signalsCaller{text: "Sign in\n\nJoin now to see", pw: true}
	sig, ok := readPageSignalsOn(f)
	if !ok || sig.Text != "Sign in Join now to see" || !sig.PasswordField {
		t.Fatalf("sig=%+v ok=%v", sig, ok)
	}
	if f.evalParam["contextId"] != 7 {
		t.Fatalf("evaluate ran outside the isolated world: %v", f.evalParam)
	}
	order := strings.Join(f.calls, ",")
	if order != "Page.getFrameTree,Page.createIsolatedWorld,Runtime.evaluate" {
		t.Fatalf("calls = %s", order)
	}
}

func TestPageSignalsBound(t *testing.T) {
	long := strings.Repeat("word \n\t ", 2000) + "\x00\x07tail"
	got := CollapsePageText(long)
	if n := len([]rune(got)); n > MaxCobrowsePageTextRunes {
		t.Fatalf("len %d", n)
	}
	if strings.ContainsAny(got, "\n\t\x00\x07") || strings.Contains(got, "  ") {
		t.Fatalf("not collapsed: %q", got[:40])
	}
	if got := CollapsePageText("  a\x00b   c\r\nd  "); got != "ab c d" {
		t.Fatalf("got %q", got)
	}
	multi := strings.Repeat("é", 3000)
	if n := len([]rune(CollapsePageText(multi))); n != MaxCobrowsePageTextRunes {
		t.Fatalf("multibyte len %d", n)
	}
	f := &signalsCaller{text: long}
	sig, ok := readPageSignalsOn(f)
	if !ok || len([]rune(sig.Text)) > MaxCobrowsePageTextRunes {
		t.Fatalf("len %d ok=%v", len([]rune(sig.Text)), ok)
	}
}

func TestPageSignalsFailureOmits(t *testing.T) {
	for _, m := range []string{"Page.getFrameTree", "Page.createIsolatedWorld", "Runtime.evaluate"} {
		f := &signalsCaller{text: "x", failOn: m}
		if sig, ok := readPageSignalsOn(f); ok || sig != (PageSignals{}) {
			t.Fatalf("%s: sig=%+v ok=%v", m, sig, ok)
		}
	}
}

// signalsEnv wires a manager with one node-wide session and a fake connection.
func signalsEnv(t *testing.T, f *signalsCaller, gate func(context.Context, pin) error) (*CobrowseSessionManager, *cobrowseSession, *int) {
	t.Helper()
	oldGate, oldConn := pageSignalsGate, pageSignalsConnect
	t.Cleanup(func() { pageSignalsGate, pageSignalsConnect = oldGate, oldConn })
	connects := new(int)
	pageSignalsGate = gate
	pageSignalsConnect = func(ctx context.Context, p pin) (cdpCaller, func(), error) {
		*connects++
		return f, func() {}, nil
	}
	m := newCobrowseSessionManager(t.TempDir(), 2)
	s := &cobrowseSession{id: "cb-1", state: SessionRunning, proc: &cobrowseProc{debugPort: 1, exited: make(chan struct{})}}
	m.sessions[s.id] = s
	return m, s, connects
}

func okGate(context.Context, pin) error { return nil }

func TestWithPageSignalsAttaches(t *testing.T) {
	f := &signalsCaller{text: "Join now to see", pw: true}
	m, _, _ := signalsEnv(t, f, okGate)
	st := m.WithPageSignals(CobrowseSessionStatus{ID: "cb-1", State: SessionRunning})
	if st.PageText != "Join now to see" || st.PasswordField == nil || !*st.PasswordField {
		t.Fatalf("st=%+v", st)
	}
	raw, _ := json.Marshal(st)
	if !strings.Contains(string(raw), `"page_text"`) || !strings.Contains(string(raw), `"password_field":true`) {
		t.Fatalf("json %s", raw)
	}
	f2 := &signalsCaller{text: "hello"}
	m2, _, _ := signalsEnv(t, f2, okGate)
	st = m2.WithPageSignals(CobrowseSessionStatus{ID: "cb-1"})
	raw, _ = json.Marshal(st)
	if !strings.Contains(string(raw), `"password_field":false`) {
		t.Fatalf("false must be reported: %s", raw)
	}
}

func TestWithPageSignalsNothingDuringHandoff(t *testing.T) {
	f := &signalsCaller{text: "secret", pw: true}
	m, s, connects := signalsEnv(t, f, okGate)
	s.explicitHandoff = true
	// The session itself refuses even when the status the caller holds is stale.
	st := m.WithPageSignals(CobrowseSessionStatus{ID: "cb-1", Driver: DriverAI})
	if st.PageText != "" || st.PasswordField != nil || *connects != 0 || len(f.calls) != 0 {
		t.Fatalf("read during handoff: st=%+v connects=%d calls=%v", st, *connects, f.calls)
	}
	st = m.WithPageSignals(CobrowseSessionStatus{ID: "cb-1", Driver: DriverHuman})
	if st.PageText != "" || st.PasswordField != nil || *connects != 0 {
		t.Fatalf("read for a human-driven status: %+v", st)
	}
}

func TestWithPageSignalsNothingForRefusedPage(t *testing.T) {
	for _, gateErr := range []error{ErrPageRefused, ErrTargetChanged} {
		f := &signalsCaller{text: "x", pw: true}
		m, _, connects := signalsEnv(t, f, func(context.Context, pin) error { return gateErr })
		st := m.WithPageSignals(CobrowseSessionStatus{ID: "cb-1"})
		if st.PageText != "" || st.PasswordField != nil || *connects != 0 || len(f.calls) != 0 {
			t.Fatalf("%v: read a refused page: %+v", gateErr, st)
		}
	}
}

func TestWithPageSignalsNotForTrackingOrPending(t *testing.T) {
	f := &signalsCaller{text: "x", pw: true}
	m, s, connects := signalsEnv(t, f, okGate)
	s.tracking = true
	if st := m.WithPageSignals(CobrowseSessionStatus{ID: "cb-1"}); st.PageText != "" || st.PasswordField != nil {
		t.Fatalf("tracking session reported signals: %+v", st)
	}
	s.tracking = false
	if st := m.WithPageSignals(CobrowseSessionStatus{ID: "cb-1", FirstLoad: FirstLoadPending}); st.PasswordField != nil {
		t.Fatalf("pending first load reported signals: %+v", st)
	}
	if st := m.WithPageSignals(CobrowseSessionStatus{ID: "gone"}); st.PasswordField != nil {
		t.Fatal("unknown session")
	}
	if *connects != 0 {
		t.Fatalf("connects = %d", *connects)
	}
}

func TestWithPageSignalsFailureOmits(t *testing.T) {
	f := &signalsCaller{text: "x", failOn: "Runtime.evaluate"}
	m, _, _ := signalsEnv(t, f, okGate)
	st := m.WithPageSignals(CobrowseSessionStatus{ID: "cb-1", URL: "https://x.example/"})
	if st.PageText != "" || st.PasswordField != nil || st.URL == "" {
		t.Fatalf("st=%+v", st)
	}
}

func TestPageSignalsNotInRegistryRecord(t *testing.T) {
	raw, _ := json.Marshal(sessionRecord{})
	if strings.Contains(string(raw), "page_text") || strings.Contains(string(raw), "password") {
		t.Fatalf("record carries page signals: %s", raw)
	}
}
