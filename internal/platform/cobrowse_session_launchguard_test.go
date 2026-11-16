package platform

import (
	"testing"
	"time"
)

// #1714: one guarded re-navigate when the browser's own start navigation has
// not committed by the end of the settle window.

func blankHistory() map[string]any {
	return map[string]any{"currentIndex": float64(0), "entries": []any{map[string]any{"url": "about:blank"}}}
}

func startWithLaunchGuard(t *testing.T, f *fakeCDPServer, url string) (CobrowseSessionStatus, time.Duration) {
	t.Helper()
	shortSettle(t, 300*time.Millisecond)
	installFixedPortLauncher(t, f.port)
	mgr := newCobrowseSessionManager(trustedBaseDir(t), 8)
	t0 := time.Now()
	st, err := mgr.StartSession(url)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return st, time.Since(t0)
}

func TestCobrowseSessionStart_ReNavigatesOnceWhenFirstNavigationNeverCommitted(t *testing.T) {
	f := startFakeCDPServer(t)
	f.mu.Lock()
	f.history = blankHistory
	f.evaluateValue = navigateResponder(f, "about:blank", map[string]any{
		"url": "https://example.com/launch", "ready": "complete", "title": "Launched",
	})
	f.mu.Unlock()
	st, took := startWithLaunchGuard(t, f, "https://example.com/launch")
	calls := f.callsFor("Page.navigate")
	if len(calls) != 1 || calls[0].Params["url"] != "https://example.com/launch" {
		t.Fatalf("Page.navigate calls = %v, want exactly one to the start URL", calls)
	}
	if st.URL != "https://example.com/launch" || st.Title != "Launched" {
		t.Fatalf("url/title = %q / %q, want the target page", st.URL, st.Title)
	}
	if took > navigateSettleWindow+cobrowseReportReserve {
		t.Fatalf("start took %v, beyond the settle window plus reserve", took)
	}
	// The history was read before the re-issue, not after.
	var order []string
	f.mu.Lock()
	for _, c := range f.calls {
		if c.Method == "Page.getNavigationHistory" || c.Method == "Page.navigate" {
			order = append(order, c.Method)
		}
	}
	f.mu.Unlock()
	if len(order) < 2 || order[0] != "Page.getNavigationHistory" || order[1] != "Page.navigate" {
		t.Fatalf("call order = %v, want the history check before the navigate", order)
	}
}

func TestCobrowseSessionStart_NoReNavigateWhenCommitUnprovenOrVisible(t *testing.T) {
	twoEntries := func() map[string]any {
		return map[string]any{"currentIndex": float64(1), "entries": []any{
			map[string]any{"url": "about:blank"}, map[string]any{"url": "https://example.com/launch"}}}
	}
	cases := []struct {
		name    string
		history func() map[string]any
		events  func(string) []map[string]any
	}{
		{"committed late: history has the target", twoEntries, nil},
		{"history unavailable", nil, nil},
		{"history entry is not about:blank", func() map[string]any {
			return map[string]any{"currentIndex": float64(0), "entries": []any{map[string]any{"url": "https://example.com/launch"}}}
		}, nil},
		{"document request in flight", blankHistory, func(m string) []map[string]any {
			if m == "Network.enable" {
				return []map[string]any{{"method": "Network.requestWillBeSent", "params": map[string]any{"type": "Document"}}}
			}
			return nil
		}},
		{"frame navigation started", blankHistory, func(m string) []map[string]any {
			if m == "Runtime.evaluate" {
				return []map[string]any{{"method": "Page.frameStartedLoading", "params": map[string]any{}}}
			}
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startFakeCDPServer(t)
			f.mu.Lock()
			f.history = tc.history
			f.eventsAfter = tc.events
			f.evaluateValue = func(string) any { return map[string]any{"url": "about:blank", "ready": "complete", "title": ""} }
			f.mu.Unlock()
			st, _ := startWithLaunchGuard(t, f, "https://example.com/launch")
			if n := len(f.callsFor("Page.navigate")); n != 0 {
				t.Fatalf("re-navigated %d time(s); a duplicate GET of a possibly single-use URL", n)
			}
			if st.URL != "about:blank" {
				t.Fatalf("url = %q, want the unchanged state returned as before", st.URL)
			}
		})
	}
}
