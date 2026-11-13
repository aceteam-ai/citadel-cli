//go:build linux

// internal/platform/cobrowse_realbrowser_linux_test.go
//
// Real-browser lifecycle tests for the Linux co-browse runner (citadel-cli#1546).
// Issue #1546 took four fix rounds because the unit tests used fakes that real
// Chrome did not match: Chrome moves itself into an app-*.scope, and with a
// fixed --remote-debugging-port it writes no DevToolsActivePort. These tests
// drive the real xvfbRunner (managed Xvfb plus Chrome on a fixed debug port).
//
// They are opt-in through CITADEL_REAL_BROWSER_TESTS=1, which only the
// "Linux co-browse (real browser)" CI job sets, so the default go test never
// starts a browser. Every test name contains "Cobrowse".
package platform

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// realBrowserEnv opts into the real-browser tests.
const realBrowserEnv = "CITADEL_REAL_BROWSER_TESTS"

// requireRealBrowser skips unless the switch is on, and fails (rather than
// skips) when it is on but the machine cannot run the tests, so a broken CI
// image is loud.
func requireRealBrowser(t *testing.T) {
	t.Helper()
	if os.Getenv(realBrowserEnv) != "1" {
		t.Skipf("set %s=1 to run real-browser co-browse tests", realBrowserEnv)
	}
	if _, err := findChromium(); err != nil {
		t.Fatalf("%s is set but no Chrome/Chromium was found: %v", realBrowserEnv, err)
	}
	if !isCommandAvailable("Xvfb") {
		t.Fatalf("%s is set but Xvfb is not installed", realBrowserEnv)
	}
}

// realEnv is one isolated session base dir plus registry dir.
type realEnv struct {
	base, reg string
}

func newRealEnv(t *testing.T) realEnv {
	t.Helper()
	requireRealBrowser(t)
	t.Setenv("CITADEL_COBROWSE_CDP_WAIT_SECONDS", "90")
	root := t.TempDir()
	e := realEnv{base: filepath.Join(root, "sessions"), reg: filepath.Join(root, "registry")}
	if err := os.MkdirAll(e.base, 0o700); err != nil {
		t.Fatal(err)
	}
	prev := registryDirResolver
	registryDirResolver = func() string { return e.reg }
	t.Cleanup(func() { registryDirResolver = prev })
	return e
}

// manager builds a node-wide style manager (it keeps registry records and can
// recover them) over the env's dirs. Calling it again simulates a fresh
// in-process manager, as after an exec restart.
func (e realEnv) manager() *CobrowseSessionManager {
	m := newCobrowseSessionManager(e.base, 2)
	m.useResolver = true
	return m
}

func testPage(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>real browser page</title><h1 id="h">hello co-browse</h1>`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// startReal starts a session and registers cleanup that stops it through m
// unless the test has already stopped or handed it off.
func startReal(t *testing.T, m *CobrowseSessionManager, url string) CobrowseSessionStatus {
	t.Helper()
	st, err := m.StartSession(url)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(st.ID) })
	return st
}

// pidGone reports whether pid no longer exists as a live process.
func pidGone(pid int) bool {
	_, alive, err := procIdentity(pid)
	return err != nil || !alive
}

// procsMentioning lists PIDs whose command line contains needle.
func procsMentioning(needle string) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), needle) {
			out = append(out, pid)
		}
	}
	return out
}

func waitUntilReal(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return cond()
}

func TestCobrowseRealBrowserLaunchNavigateScreenshot(t *testing.T) {
	e := newRealEnv(t)
	m := e.manager()
	st := startReal(t, m, testPage(t))

	rec, err := loadRecord(e.reg, st.ID)
	if err != nil {
		t.Fatalf("no registry record after launch: %v", err)
	}
	if rec.BrowserID == "" || rec.DebugPort == 0 || rec.BrowserPID == 0 || rec.XvfbPID == 0 {
		t.Fatalf("incomplete record: %+v", rec)
	}
	// Documents the Chrome behavior #1546 tripped over; informational only.
	if _, err := os.Stat(filepath.Join(st.Profile, devToolsActivePortFile)); err == nil {
		t.Log("Chrome wrote DevToolsActivePort with a fixed port on this version")
	} else {
		t.Log("Chrome wrote no DevToolsActivePort with a fixed port (expected)")
	}

	shot, err := m.Screenshot(st.ID)
	if err != nil || len(shot) < 100 {
		t.Fatalf("Screenshot: len=%d err=%v", len(shot), err)
	}
}

func TestCobrowseRealBrowserReadoptInSameProcess(t *testing.T) {
	e := newRealEnv(t)
	m1 := e.manager()
	st := startReal(t, m1, testPage(t))

	rec, err := loadRecord(e.reg, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The exec case: owner PID and start time are this process's.
	if rec.OwnerPID != os.Getpid() || rec.OwnerStart == 0 || rec.OwnerStart != currentOwnerIdentity() {
		t.Fatalf("record owner is not this process: %+v", rec)
	}
	// The recorded id must pass against the real Chrome, and a wrong one fail.
	if err := defaultAdoptCDPCheck(rec.DebugPort, st.Profile, rec.BrowserID); err != nil {
		t.Fatalf("browser id check against real Chrome: %v", err)
	}
	if err := defaultAdoptCDPCheck(rec.DebugPort, st.Profile, "not-the-id"); err == nil {
		t.Fatal("a wrong browser id must be refused")
	}

	// Drop m1 (never stopped here: the browser must survive) and recover in a
	// brand new manager.
	m2 := e.manager()
	m2.Recover()
	got, ok := m2.SessionStatus(st.ID)
	if !ok {
		t.Fatalf("session %s was not re-adopted", st.ID)
	}
	if got.State != SessionRunning || got.Generation != 2 {
		t.Fatalf("adopted status: state=%v generation=%d", got.State, got.Generation)
	}
	if _, err := m2.Screenshot(st.ID); err != nil {
		t.Fatalf("screenshot through the adopted session: %v", err)
	}
	if err := m2.Stop(st.ID); err != nil {
		t.Fatalf("stop adopted session: %v", err)
	}
	if !waitUntilReal(15*time.Second, func() bool { return pidGone(rec.BrowserPID) && pidGone(rec.XvfbPID) }) {
		t.Fatal("browser or Xvfb survived stopping the adopted session")
	}
}

func TestCobrowseRealBrowserRefusesMismatchedBrowserID(t *testing.T) {
	e := newRealEnv(t)
	m1 := e.manager()
	st := startReal(t, m1, testPage(t))

	rec, err := loadRecord(e.reg, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec.BrowserID = "0000-recorded-id-that-chrome-does-not-have"
	if err := saveRecord(e.reg, rec); err != nil {
		t.Fatal(err)
	}

	m2 := e.manager()
	m2.Recover()
	if _, ok := m2.SessionStatus(st.ID); ok {
		t.Fatal("a session whose recorded browser id does not match was adopted")
	}
	after, err := loadRecord(e.reg, st.ID)
	if err != nil {
		t.Fatalf("record should be kept as expired: %v", err)
	}
	if after.State != RecordExpired {
		t.Fatalf("record state = %q, want %q", after.State, RecordExpired)
	}
}

func TestCobrowseRealBrowserStopLeavesNoProcesses(t *testing.T) {
	e := newRealEnv(t)
	m := e.manager()
	st := startReal(t, m, testPage(t))

	rec, err := loadRecord(e.reg, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(procsMentioning(st.Profile)) == 0 {
		t.Fatal("no process mentions the session profile while it runs; the leak check below would prove nothing")
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !waitUntilReal(15*time.Second, func() bool {
		return pidGone(rec.BrowserPID) && pidGone(rec.XvfbPID) && len(procsMentioning(st.Profile)) == 0
	}) {
		t.Fatalf("processes left after Stop: browser=%v xvfb=%v profile users=%v",
			!pidGone(rec.BrowserPID), !pidGone(rec.XvfbPID), procsMentioning(st.Profile))
	}
	if !waitUntilReal(5*time.Second, func() bool { _, err := os.Lstat(st.Profile); return os.IsNotExist(err) }) {
		var left []string
		_ = filepath.WalkDir(st.Profile, func(p string, _ os.DirEntry, _ error) error { left = append(left, p); return nil })
		if len(left) > 40 {
			left = left[:40]
		}
		t.Fatalf("session dir still present; entries: %v", left)
	}
	if _, err := loadRecord(e.reg, st.ID); err == nil {
		t.Fatal("registry record left behind after Stop")
	}
}

// The systemd-scope launch path needs a user systemd manager, which GitHub
// hosted runners do not have. planCobrowseScope, the app-*.scope detection and
// the scope stop logic stay covered by cobrowse_scope_test.go.
func TestCobrowseRealBrowserSystemdScope(t *testing.T) {
	t.Skip("systemd scope path cannot run on GitHub-hosted runners (no user systemd manager); covered by cobrowse_scope_test.go")
}
