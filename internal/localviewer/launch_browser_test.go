package localviewer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// TestLaunchFileOpensViewerInRealBrowser follows the real opening path in a
// real browser (Windows CI: Chrome and Edge): the browser is started on the
// one-time launch file (file:), which forwards to /open (a cross-site top-level
// navigation), which sets the Strict cookie, stores the token and replaces
// itself with "/". The viewer page then polls /api/state with the cookie and
// the token, which is only possible if every step worked.
func TestLaunchFileOpensViewerInRealBrowser(t *testing.T) {
	if os.Getenv("CITADEL_COBROWSE_BROWSER_IT") == "" {
		t.Skip("set CITADEL_COBROWSE_BROWSER_IT=1 to run the real-browser launch-file test")
	}
	info, err := platform.DiscoverCobrowseBrowser()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	h := newHarness(t)
	launch, _, err := h.srv.MintLaunchFile()
	if err != nil {
		t.Fatal(err)
	}
	// The browser is started directly on the file, the way the OS opener
	// does it; co-browse sessions refuse file: URLs by design.
	fileURL := "file:///" + strings.TrimPrefix(filepath.ToSlash(launch), "/")
	cmd := exec.Command(info.Path,
		"--headless=new", "--no-first-run", "--no-default-browser-check",
		"--user-data-dir="+filepath.Join(t.TempDir(), "profile"),
		"--disable-background-networking", "--", fileURL)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		h.srv.mu.Lock()
		polled := !h.srv.lastPoll.IsZero()
		h.srv.mu.Unlock()
		if polled {
			if _, err := os.Stat(launch); !os.IsNotExist(err) {
				t.Fatal("the launch file must be removed once its code is used")
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("the viewer page never polled /api/state with the cookie and token after opening the launch file")
}
