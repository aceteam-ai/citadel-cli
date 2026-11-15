package localviewer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// TestViewerPageInRealBrowser loads the viewer page in a real headless browser
// (the Windows CI job runs it with Chrome and with Edge). It proves the page's
// script runs under its CSP, the code-for-cookie exchange works in a browser,
// and that a real click on Approve or Take control passes the Origin, cookie
// and CSRF checks.
func TestViewerPageInRealBrowser(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 to run the real-browser viewer test")
	}
	h := newHarness(t)
	ctx := context.Background()
	st, err := h.svc.Start(ctx, "", "Fixture goal", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Click(ctx, st.SessionID, "#x", nil, nil, "g"); err == nil {
		t.Fatal("write must need approval")
	}

	m := platform.NewCobrowseSessionManager(filepath.Join(t.TempDir(), "sessions"), 1)
	defer m.StopAll()
	page, err := m.StartSession(h.srv.MintURL())
	if err != nil {
		t.Fatalf("start viewer browser: %v", err)
	}

	waitText := func(sel, want string) {
		t.Helper()
		// 60s: a cold Edge on the Windows runner took over 20s to paint the
		// viewer once (a timing flake, not a viewer defect).
		deadline := time.Now().Add(60 * time.Second)
		var last string
		for time.Now().Before(deadline) {
			if res, err := m.ExtractBounded(page.ID, sel, nil, 500); err == nil {
				last = res.Text
				if strings.Contains(last, want) {
					return
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s: got %q, want %q", sel, last, want)
	}
	waitText("#session-goal", "Fixture goal")
	waitText("#agent-line", "No agent connected")
	waitText("#approval", "Allow this agent")

	// A selector click can land before layout settles (the known #13280
	// race, owned by S2), so retry the click until the server sees it.
	clickUntil(t, m, page.ID, "#approve", func() bool { return h.svc.Sessions(st.SessionID)[0].Approval == "approved" })

	waitText("#drive", "Take control")
	clickUntil(t, m, page.ID, "#drive", func() bool { return h.svc.Sessions(st.SessionID)[0].Driver == "human" })
	waitText("#waiting", "You are driving")
}

func clickUntil(t *testing.T, m *platform.CobrowseSessionManager, id, sel string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.Click(id, sel, nil, nil); err != nil {
			t.Logf("click %s: %v", sel, err)
		}
		for i := 0; i < 10; i++ {
			if cond() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	t.Fatalf("clicking %s never took effect", sel)
}
