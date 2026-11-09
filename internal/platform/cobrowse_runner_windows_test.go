//go:build windows

package platform

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// helperEnv selects a mode for TestWindowsHelperProcess, which re-runs this test
// binary as a separate owner process that a test can kill.
const helperEnv = "CITADEL_COBROWSE_TEST_HELPER"

// browserITEnv opts into the real-browser integration tests. CI sets it once
// per browser, with EnvCobrowseBrowser pointing at Chrome or Edge.
const browserITEnv = "CITADEL_COBROWSE_BROWSER_IT"

// TestWindowsHelperProcess is not a test on its own. With helperEnv set it
// becomes an owner process:
//   - "job": starts a ping tree inside a kill-on-close job, prints the child
//     PID, and blocks.
//   - "session": starts a real browser session under the base dir in
//     CITADEL_COBROWSE_TEST_BASE, prints "<browserPID> <sessionDir>", and blocks.
func TestWindowsHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	switch mode {
	case "job":
		cmd := exec.Command("cmd.exe", "/c", "ping -n 600 127.0.0.1 >NUL")
		if _, err := startInKillOnCloseJob(cmd); err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		child := waitForChildPID(uint32(cmd.Process.Pid), 10*time.Second)
		fmt.Println(child)
	case "session":
		m := newCobrowseSessionManager(os.Getenv("CITADEL_COBROWSE_TEST_BASE"), 2)
		st, err := m.StartSession("about:blank")
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		_, bpid, _ := readSessionPidfile(st.Profile)
		fmt.Println(bpid, st.Profile)
	}
	time.Sleep(10 * time.Minute)
	os.Exit(0)
}

func startHelper(t *testing.T, mode string, extraEnv ...string) (*exec.Cmd, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsHelperProcess$", "-test.v=false")
	cmd.Env = append(append(os.Environ(), helperEnv+"="+mode), extraEnv...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			txt := strings.TrimSpace(sc.Text())
			if txt != "" {
				line <- txt
				return
			}
		}
		close(line)
	}()
	select {
	case l, ok := <-line:
		if !ok || strings.HasPrefix(l, "ERR") {
			t.Fatalf("helper %s failed: %q", mode, l)
		}
		return cmd, l
	case <-time.After(90 * time.Second):
		t.Fatalf("helper %s did not report", mode)
	}
	return nil, ""
}

// waitForChildPID returns the PID of a process whose parent is parent.
func waitForChildPID(parent uint32, timeout time.Duration) uint32 {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
		if err == nil {
			var pe windows.ProcessEntry32
			pe.Size = uint32(unsafe.Sizeof(pe))
			for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
				if pe.ParentProcessID == parent {
					_ = windows.CloseHandle(snap)
					return pe.ProcessID
				}
			}
			_ = windows.CloseHandle(snap)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0
}

func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAliveWithStart(pid, 0) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %s", pid, timeout)
}

func TestWindowsJobStopKillsProcessTree(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 600 127.0.0.1 >NUL")
	job, err := startInKillOnCloseJob(cmd)
	if err != nil {
		t.Fatalf("start in job: %v", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	child := waitForChildPID(uint32(cmd.Process.Pid), 10*time.Second)
	if child == 0 {
		t.Fatal("child ping never started (process was not resumed?)")
	}
	if err := stopJob(job, exited); err != nil {
		t.Fatalf("stop job: %v", err)
	}
	waitGone(t, cmd.Process.Pid, 10*time.Second)
	waitGone(t, int(child), 10*time.Second)
}

// TestWindowsJobOwnerKilledKillsTree proves kill-on-close: a forced kill of the
// owner (no cleanup code runs) still terminates the processes it started.
func TestWindowsJobOwnerKilledKillsTree(t *testing.T) {
	owner, line := startHelper(t, "job")
	child, err := strconv.Atoi(line)
	if err != nil || child == 0 {
		t.Fatalf("bad child pid %q", line)
	}
	if !processAliveWithStart(child, 0) {
		t.Fatal("child not running before owner kill")
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	waitGone(t, child, 15*time.Second)
}

func TestWindowsBaseDirIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cobrowse-sessions")
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatalf("re-validate fresh dir: %v", err)
	}

	// A plain dir inherits its parent's ACL (and, for an elevated process, is
	// owned by Administrators); either way it must be refused, not repaired.
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureCobrowseBaseDir(plain); err == nil ||
		!(strings.Contains(err.Error(), "inherits") || strings.Contains(err.Error(), "is owned by")) {
		t.Fatalf("plain dir must be refused, got %v", err)
	}

	// An extra grant to Everyone must be refused.
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(user)}},
		{AccessPermissions: windows.GENERIC_READ, AccessMode: windows.SET_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(everyone)}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := ensureCobrowseBaseDir(dir); err == nil || !strings.Contains(err.Error(), "grants access") {
		t.Fatalf("extra grant must be refused, got %v", err)
	}
}

func TestWindowsSweepRemovesOnlyDeadOwners(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	if err := ensureCobrowseBaseDir(base); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) string {
		d := filepath.Join(base, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Dead owner: a process that has already exited.
	dead := exec.Command("cmd.exe", "/c", "exit 0")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadDir := mk("cb-dead")
	writeSessionPidfile(deadDir, dead.Process.Pid, 0, 0, 1234)

	// Live owner with the matching creation time: kept.
	liveCmd := exec.Command("cmd.exe", "/c", "ping -n 120 127.0.0.1 >NUL")
	if err := liveCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = liveCmd.Process.Kill(); _ = liveCmd.Wait() })
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(liveCmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	liveStart, err := processStartToken(h)
	_ = windows.CloseHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	liveDir := mk("cb-live")
	writeSessionPidfile(liveDir, liveCmd.Process.Pid, 0, 0, liveStart)

	// Live PID but a different creation time: a reused PID, so the dir is stale.
	reusedDir := mk("cb-reused")
	writeSessionPidfile(reusedDir, liveCmd.Process.Pid, 0, 0, liveStart-1)

	// No pidfile yet and fresh: a sibling mid-launch, kept.
	launchingDir := mk("cb-launching")

	// Our own PID but another start time: a dead predecessor whose PID we
	// reused, swept. Our own PID and start time: ours, kept.
	selfReusedDir := mk("cb-self-reused")
	writeSessionPidfile(selfReusedDir, os.Getpid(), 0, 0, currentProcessStartToken()-1)
	selfDir := mk("cb-self")
	writeSessionPidfile(selfDir, os.Getpid(), 0, 0, currentProcessStartToken())

	sweepWindowsSessions(base)

	for dir, wantExists := range map[string]bool{deadDir: false, liveDir: true, reusedDir: false, launchingDir: true, selfReusedDir: false, selfDir: true} {
		_, err := os.Stat(dir)
		if exists := err == nil; exists != wantExists {
			t.Errorf("%s: exists=%v want %v", filepath.Base(dir), exists, wantExists)
		}
	}
	if !processAliveWithStart(liveCmd.Process.Pid, liveStart) {
		t.Error("sweep must never kill a process")
	}
}

// --- Real-browser integration (opt-in; CI runs it once for Chrome, once for Edge).

func requireBrowserIT(t *testing.T) {
	t.Helper()
	if os.Getenv(browserITEnv) == "" {
		t.Skipf("set %s=1 (and optionally %s) to run the real-browser test", browserITEnv, EnvCobrowseBrowser)
	}
}

// fixtureUA records the User-Agent header of the last fixture request.
var fixtureUA atomic.Value

func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixtureUA.Store(r.UserAgent())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/two":
			fmt.Fprint(w, `<!doctype html><title>two</title><h1 id="t">Fixture page two</h1>`)
		default:
			fmt.Fprint(w, `<!doctype html><title>one</title><h1 id="t">Fixture page one</h1><a id="next" href="/two">next</a>`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWindowsRealBrowserSession(t *testing.T) {
	requireBrowserIT(t)
	srv := fixtureServer(t)
	info, err := DiscoverCobrowseBrowser()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	t.Logf("browser: %s (%s)", info.Path, info.Kind)

	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	m := newCobrowseSessionManager(base, 2)
	st, err := m.StartSession(srv.URL + "/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	if st.Browser != info.Kind {
		t.Errorf("status browser %q, discovered %q", st.Browser, info.Kind)
	}
	if want := os.Getenv("CITADEL_COBROWSE_EXPECT_KIND"); want != "" && st.Browser != want {
		t.Fatalf("launched %s, CI expected %s", st.Browser, want)
	}

	assertDebugPortLoopbackOnly(t, st.DebugPort)

	waitExtract(t, m, st.ID, "#t", "Fixture page one")
	assertNotHeadlessUA(t, st.DebugPort)
	png, err := m.Screenshot(st.ID)
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(png)
	if err != nil || !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		t.Fatalf("screenshot is not a PNG (err %v)", err)
	}
	if _, err := m.Navigate(st.ID, srv.URL+"/two"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	waitExtract(t, m, st.ID, "#t", "Fixture page two")

	_, bpid, _ := readSessionPidfile(st.Profile)
	if bpid == 0 {
		t.Fatal("no browser pid recorded")
	}
	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitGone(t, bpid, 15*time.Second)
	if _, err := os.Stat(st.Profile); !os.IsNotExist(err) {
		t.Errorf("session dir not removed on stop: %v", err)
	}
}

// TestWindowsRealBrowserOwnerKilled covers two acceptance items: killing the
// owner leaves no browser process, and the stale session dir it leaves is swept
// on the next start.
func TestWindowsRealBrowserOwnerKilled(t *testing.T) {
	requireBrowserIT(t)
	base := filepath.Join(t.TempDir(), "cobrowse-sessions")
	owner, line := startHelper(t, "session", "CITADEL_COBROWSE_TEST_BASE="+base)
	parts := strings.SplitN(line, " ", 2)
	bpid, err := strconv.Atoi(parts[0])
	if err != nil || len(parts) != 2 {
		t.Fatalf("bad helper line %q", line)
	}
	staleDir := parts[1]
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	waitGone(t, bpid, 20*time.Second)

	m := newCobrowseSessionManager(base, 2)
	st, err := m.StartSession("about:blank")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer m.StopAll()
	if st.Profile == staleDir {
		t.Fatal("new session reused the stale dir")
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Fatalf("stale session dir was not swept on the next start: %v", err)
	}
}

// assertNotHeadlessUA checks the stealth default: neither navigator.userAgent
// nor the User-Agent the page was requested with says "Headless". The client
// hint brands are logged (not asserted): --user-agent does not change them.
func assertNotHeadlessUA(t *testing.T, port int) {
	t.Helper()
	ua, err := runtimeEvaluate(port, "navigator.userAgent")
	if err != nil {
		t.Fatalf("read navigator.userAgent: %v", err)
	}
	t.Logf("navigator.userAgent: %v", ua)
	if s, _ := ua.(string); s == "" || strings.Contains(s, "Headless") {
		t.Fatalf("navigator.userAgent must not say Headless: %q", ua)
	}
	header, _ := fixtureUA.Load().(string)
	if header == "" || strings.Contains(header, "Headless") {
		t.Fatalf("request User-Agent must not say Headless: %q", header)
	}
	if ua != header {
		t.Errorf("navigator.userAgent %q differs from the request header %q", ua, header)
	}
	brands, _ := runtimeEvaluate(port, "JSON.stringify(navigator.userAgentData ? navigator.userAgentData.brands : null)")
	t.Logf("navigator.userAgentData.brands: %v", brands)
}

func waitExtract(t *testing.T, m *CobrowseSessionManager, id, sel, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		res, err := m.Extract(id, sel, nil)
		if err == nil && strings.TrimSpace(res.Text) == want {
			return
		}
		last, lastErr = res.Text, err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("extract %s: got %q err %v, want %q", sel, last, lastErr, want)
}

// assertDebugPortLoopbackOnly fails if the CDP port answers on a non-loopback
// local address, which would expose browser control to the network.
func assertDebugPortLoopbackOnly(t *testing.T, port int) {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("interface addrs: %v", err)
	}
	checked := 0
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.To4() == nil {
			continue
		}
		checked++
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ipn.IP.String(), strconv.Itoa(port)), 2*time.Second)
		if err == nil {
			_ = c.Close()
			t.Fatalf("CDP port %d is reachable on %s; it must listen on 127.0.0.1 only", port, ipn.IP)
		}
	}
	t.Logf("CDP port %d refused on %d non-loopback address(es)", port, checked)
}

func TestHeadedUserAgentFallbackCacheAndSingleflight(t *testing.T) {
	prevLaunch, prevRetry := uaProbeLaunch, uaProbeRetryAfter
	t.Cleanup(func() {
		uaProbeLaunch, uaProbeRetryAfter = prevLaunch, prevRetry
		uaCacheMu.Lock()
		uaCache = map[string]*uaEntry{}
		uaCacheMu.Unlock()
	})
	base := filepath.Join(t.TempDir(), "s")
	if err := ensureCobrowseBaseDir(base); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	release := make(chan struct{})
	uaProbeLaunch = func(BrowserInfo, string) (string, error) {
		calls.Add(1)
		<-release
		return "", errors.New("policy blocked")
	}
	info := BrowserInfo{Path: filepath.Join(t.TempDir(), "msedge.exe"), Kind: "edge"}
	results := make(chan string, 3)
	for i := 0; i < 3; i++ {
		go func() { results <- headedUserAgent(info, base) }()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for i := 0; i < 3; i++ {
		if ua := <-results; ua != "" {
			t.Fatalf("a failed probe must fall back to the browser's own UA, got %q", ua)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("concurrent callers should share one probe, got %d", n)
	}
	_ = headedUserAgent(info, base)
	if n := calls.Load(); n != 1 {
		t.Fatalf("a failed probe should be cached, got %d probes", n)
	}
	uaProbeRetryAfter = 0
	uaProbeLaunch = func(BrowserInfo, string) (string, error) {
		calls.Add(1)
		return "Mozilla/5.0 HeadlessChrome/150.0.0.0 Safari/537.36", nil
	}
	if ua := headedUserAgent(info, base); ua != "Mozilla/5.0 Chrome/150.0.0.0 Safari/537.36" {
		t.Fatalf("retry after the window: %q", ua)
	}
}

// TestWindowsRealBrowserUAWhenProbeFails forces the user-agent probe to fail
// and checks the session still does not report a headless user agent.
func TestWindowsRealBrowserUAWhenProbeFails(t *testing.T) {
	requireBrowserIT(t)
	prevLaunch := uaProbeLaunch
	uaCacheMu.Lock()
	uaCache = map[string]*uaEntry{}
	uaCacheMu.Unlock()
	uaProbeLaunch = func(BrowserInfo, string) (string, error) { return "", errors.New("forced probe failure") }
	t.Cleanup(func() {
		uaProbeLaunch = prevLaunch
		uaCacheMu.Lock()
		uaCache = map[string]*uaEntry{}
		uaCacheMu.Unlock()
	})
	srv := fixtureServer(t)
	m := newCobrowseSessionManager(filepath.Join(t.TempDir(), "cobrowse-sessions"), 2)
	st, err := m.StartSession(srv.URL + "/")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.StopAll() })
	waitExtract(t, m, st.ID, "#t", "Fixture page one")
	assertNotHeadlessUA(t, st.DebugPort)
}
