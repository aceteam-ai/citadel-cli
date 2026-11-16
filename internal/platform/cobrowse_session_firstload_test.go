package platform

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// #1714: a start URL that has not committed when the settle window ends is
// reported as a pending first load, never re-navigated.

const slowStartURL = "https://slow.example:8443/magic?token=SECRET#frag"

func TestCobrowseSessionStart_PendingFirstLoadThenClears(t *testing.T) {
	shortSettle(t, 200*time.Millisecond)
	var committed atomic.Bool
	f := startFakeCDPServer(t)
	f.mu.Lock()
	f.evaluateValue = func(string) any {
		if committed.Load() {
			return map[string]any{"url": slowStartURL, "ready": "complete", "title": "In"}
		}
		return map[string]any{"url": "about:blank", "ready": "complete", "title": ""}
	}
	f.listedURL = func() string {
		if committed.Load() {
			return slowStartURL
		}
		return "about:blank"
	}
	f.mu.Unlock()
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	st, err := m.StartSession(slowStartURL)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if st.FirstLoad != FirstLoadPending || st.PendingOrigin != "https://slow.example:8443" {
		t.Fatalf("first_load/origin = %q / %q, want pending + origin only", st.FirstLoad, st.PendingOrigin)
	}
	if strings.Contains(st.PendingOrigin, "SECRET") || strings.Contains(st.PendingOrigin, "magic") {
		t.Fatalf("origin leaks path or query: %q", st.PendingOrigin)
	}
	if got, ok := m.SessionStatus(st.ID); !ok || got.FirstLoad != FirstLoadPending {
		t.Fatalf("a status read while still blank must stay pending, got %+v", got)
	}
	committed.Store(true)
	got, ok := m.SessionStatus(st.ID)
	if !ok || got.FirstLoad != "" || got.PendingOrigin != "" || got.URL != slowStartURL {
		t.Fatalf("after the page lands: %+v, want the marker cleared", got)
	}
	if again, _ := m.SessionStatus(st.ID); again.FirstLoad != "" {
		t.Fatal("the marker came back after clearing")
	}
	if n := len(f.callsFor("Page.navigate")); n != 0 {
		t.Fatalf("Page.navigate called %d time(s); the start URL must never be re-issued", n)
	}
}

func TestCobrowseSessionStart_LoadedPageIsNotPending(t *testing.T) {
	st, f := startOnFakeCDPWithURL(t, map[string]any{
		"url": "https://example.com/", "ready": "complete", "title": "ok",
	}, "https://example.com/")
	if st.FirstLoad != "" || st.PendingOrigin != "" {
		t.Fatalf("loaded page marked pending: %+v", st)
	}
	if n := len(f.callsFor("Page.navigate")); n != 0 {
		t.Fatalf("Page.navigate called %d time(s)", n)
	}
}

// Pinning and settling share one deadline, so a start that never commits
// returns within window + reserve in total.
func TestCobrowseSessionStart_PendingFirstLoadBoundedByOneDeadline(t *testing.T) {
	shortSettle(t, 300*time.Millisecond)
	shortBudget(t, CobrowseActionDeadline, 200*time.Millisecond)
	f := startFakeCDPServer(t)
	f.mu.Lock()
	f.evaluateValue = func(string) any { return map[string]any{"url": "about:blank", "ready": "complete", "title": ""} }
	f.mu.Unlock()
	installFixedPortLauncher(t, f.port)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	t0 := time.Now()
	st, err := m.StartSession(slowStartURL)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if d := time.Since(t0); d > 300*time.Millisecond+200*time.Millisecond+250*time.Millisecond {
		t.Fatalf("start took %v, beyond window + reserve", d)
	}
	if st.FirstLoad != FirstLoadPending {
		t.Fatalf("first_load = %q, want pending", st.FirstLoad)
	}
	if n := len(f.callsFor("Page.navigate")); n != 0 {
		t.Fatalf("Page.navigate called %d time(s)", n)
	}
}
