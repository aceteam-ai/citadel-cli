package platform

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// containsArg reports whether args contains an exact match for want.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// hasPrefixArg reports whether any arg starts with prefix.
func hasPrefixArg(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// countPrefixArg counts args starting with prefix.
func countPrefixArg(args []string, prefix string) int {
	n := 0
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			n++
		}
	}
	return n
}

// TestBuildChromeArgs_Invariants checks the flags that must hold in BOTH stealth
// modes: the CDP debug wiring, the persistent profile, the single merged
// --disable-features (Chrome only honors the last one), and that the automation
// switch is never emitted (no "controlled by automated test software" infobar).
func TestBuildChromeArgs_Invariants(t *testing.T) {
	for _, stealth := range []bool{true, false} {
		args := buildChromeArgs(cobrowseLaunchOptions{
			debugPort:  9222,
			profileDir: "/tmp/profile",
			stealth:    stealth,
		})
		if !containsArg(args, "--remote-debugging-port=9222") {
			t.Errorf("stealth=%v: missing --remote-debugging-port=9222 in %v", stealth, args)
		}
		if !containsArg(args, "--remote-debugging-address=127.0.0.1") {
			t.Errorf("stealth=%v: missing --remote-debugging-address=127.0.0.1", stealth)
		}
		if !containsArg(args, "--user-data-dir=/tmp/profile") {
			t.Errorf("stealth=%v: missing persistent --user-data-dir", stealth)
		}
		// Exactly one --disable-features, and it must still carry Translate.
		if n := countPrefixArg(args, "--disable-features="); n != 1 {
			t.Errorf("stealth=%v: want exactly 1 --disable-features=, got %d in %v", stealth, n, args)
		}
		if !hasPrefixArg(args, "--disable-features=") {
			t.Errorf("stealth=%v: missing --disable-features=", stealth)
		}
		for _, a := range args {
			if strings.HasPrefix(a, "--disable-features=") && !strings.Contains(a, "Translate") {
				t.Errorf("stealth=%v: --disable-features dropped Translate: %q", stealth, a)
			}
		}
		// Never emit the automation switch (would re-add the infobar / webdriver).
		if containsArg(args, "--enable-automation") {
			t.Errorf("stealth=%v: --enable-automation must never be emitted", stealth)
		}
		// excludeSwitches is a chromedriver capability, not a CLI flag.
		if hasPrefixArg(args, "--exclude-switches") {
			t.Errorf("stealth=%v: --exclude-switches is not a valid Chrome flag", stealth)
		}
	}
}

// TestBuildChromeArgs_StealthToggle checks the stealth-only flags appear when on
// and are absent when off.
func TestBuildChromeArgs_StealthToggle(t *testing.T) {
	on := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true})
	if !containsArg(on, "--disable-blink-features=AutomationControlled") {
		t.Errorf("stealth on: missing --disable-blink-features=AutomationControlled in %v", on)
	}
	if !containsArg(on, "--lang=en-US") {
		t.Errorf("stealth on: missing default --lang=en-US in %v", on)
	}

	off := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: false})
	if containsArg(off, "--disable-blink-features=AutomationControlled") {
		t.Errorf("stealth off: --disable-blink-features must be absent in %v", off)
	}
	if hasPrefixArg(off, "--lang=") {
		t.Errorf("stealth off: --lang must be absent in %v", off)
	}
}

// TestBuildChromeArgs_UserAgentAndLang checks optional overrides: a custom UA is
// emitted only when set, and an empty UA leaves real Chrome's own UA in place.
func TestBuildChromeArgs_UserAgentAndLang(t *testing.T) {
	withUA := buildChromeArgs(cobrowseLaunchOptions{
		debugPort: 9222, profileDir: "/p", stealth: true,
		lang: "fr-FR", userAgent: "Mozilla/5.0 Custom",
	})
	if !containsArg(withUA, "--user-agent=Mozilla/5.0 Custom") {
		t.Errorf("expected custom --user-agent in %v", withUA)
	}
	if !containsArg(withUA, "--lang=fr-FR") {
		t.Errorf("expected custom --lang=fr-FR in %v", withUA)
	}

	noUA := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true})
	if hasPrefixArg(noUA, "--user-agent=") {
		t.Errorf("expected no --user-agent when unset (use real Chrome UA), got %v", noUA)
	}
}

// TestBuildChromeArgs_StartURL checks the start URL is the last arg when set and
// omitted when empty.
func TestBuildChromeArgs_StartURL(t *testing.T) {
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort: 9222, profileDir: "/p", stealth: true, startURL: "https://example.com",
	})
	if got := args[len(args)-1]; got != "https://example.com" {
		t.Errorf("expected start URL last, got %q in %v", got, args)
	}

	none := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true})
	for _, a := range none {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			t.Errorf("expected no URL arg when startURL empty, found %q", a)
		}
	}
}

// TestStealthEnabled checks the env toggle: ON by default, OFF for known
// falsey values.
func TestStealthEnabled(t *testing.T) {
	t.Setenv(EnvCobrowseStealth, "")
	if !stealthEnabled() {
		t.Error("stealth should default ON when env unset")
	}
	for _, v := range []string{"0", "false", "off", "no", "OFF", " false "} {
		t.Setenv(EnvCobrowseStealth, v)
		if stealthEnabled() {
			t.Errorf("stealth should be OFF for %q", v)
		}
	}
	for _, v := range []string{"1", "true", "on", "yes", "anything"} {
		t.Setenv(EnvCobrowseStealth, v)
		if !stealthEnabled() {
			t.Errorf("stealth should be ON for %q", v)
		}
	}
}

// TestBuildChromeArgs_SoftwareGL checks --disable-gpu is emitted only when the
// browser runs on a managed (GPU-less) Xvfb virtual display.
func TestBuildChromeArgs_SoftwareGL(t *testing.T) {
	on := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true, softwareGL: true})
	if !containsArg(on, "--disable-gpu") {
		t.Errorf("softwareGL on: missing --disable-gpu in %v", on)
	}
	off := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true})
	if containsArg(off, "--disable-gpu") {
		t.Errorf("softwareGL off: --disable-gpu must be absent in %v", off)
	}
}

// TestBuildChromeArgs_PasswordStoreBasic checks --password-store=basic is emitted
// only when passwordStoreBasic is set. The meeting bot sets it so its persistent,
// externally-seeded profile uses Chromium's build-independent os_crypt key
// instead of a keyring secret; co-browse leaves it off to keep keyring-backed
// persistent logins.
func TestBuildChromeArgs_PasswordStoreBasic(t *testing.T) {
	on := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true, passwordStoreBasic: true})
	if !containsArg(on, "--password-store=basic") {
		t.Errorf("passwordStoreBasic on: missing --password-store=basic in %v", on)
	}
	off := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true})
	if containsArg(off, "--password-store=basic") {
		t.Errorf("passwordStoreBasic off: --password-store=basic must be absent in %v", off)
	}
	// Default co-browse launch options (as constructed in Start) must NOT enable it.
	cobrowseLike := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true, softwareGL: true})
	if containsArg(cobrowseLike, "--password-store=basic") {
		t.Errorf("co-browse default: --password-store=basic must be absent in %v", cobrowseLike)
	}
}

// TestBuildChromeArgs_AutoplayNoUserGesture checks
// --autoplay-policy=no-user-gesture-required is emitted only when the option is
// set. The meeting bot sets it so Chrome plays incoming (WebRTC) call audio into
// the recorder's sink instead of recording silence (issue #5098); co-browse
// leaves it off since a human drives it and supplies the gesture.
func TestBuildChromeArgs_AutoplayNoUserGesture(t *testing.T) {
	on := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true, autoplayNoUserGesture: true})
	if !containsArg(on, "--autoplay-policy=no-user-gesture-required") {
		t.Errorf("autoplayNoUserGesture on: missing --autoplay-policy=no-user-gesture-required in %v", on)
	}
	off := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true})
	if containsArg(off, "--autoplay-policy=no-user-gesture-required") {
		t.Errorf("autoplayNoUserGesture off: --autoplay-policy must be absent in %v", off)
	}
	// Default co-browse launch options must NOT enable it.
	cobrowseLike := buildChromeArgs(cobrowseLaunchOptions{debugPort: 9222, profileDir: "/p", stealth: true, softwareGL: true})
	if containsArg(cobrowseLike, "--autoplay-policy=no-user-gesture-required") {
		t.Errorf("co-browse default: --autoplay-policy must be absent in %v", cobrowseLike)
	}
}

// TestResolveCobrowseDisplay checks the display-mode precedence: a dedicated
// managed Xvfb by default, an operator-pinned display when EnvCobrowseDisplay is
// set (trimmed).
func TestResolveCobrowseDisplay(t *testing.T) {
	t.Setenv(EnvCobrowseDisplay, "")
	if mode, d := resolveCobrowseDisplay(); mode != displayManaged || d != "" {
		t.Errorf("unset: want (managed, \"\"), got (%v, %q)", mode, d)
	}
	t.Setenv(EnvCobrowseDisplay, "  :0 ")
	if mode, d := resolveCobrowseDisplay(); mode != displayExplicit || d != ":0" {
		t.Errorf("set :0: want (explicit, \":0\"), got (%v, %q)", mode, d)
	}
}

// TestXvfbResolution checks the geometry override falls back to the default.
func TestXvfbResolution(t *testing.T) {
	t.Setenv(EnvCobrowseResolution, "")
	if got := xvfbResolution(); got != defaultXvfbResolution {
		t.Errorf("unset: want %q, got %q", defaultXvfbResolution, got)
	}
	t.Setenv(EnvCobrowseResolution, "1280x720x24")
	if got := xvfbResolution(); got != "1280x720x24" {
		t.Errorf("set: want 1280x720x24, got %q", got)
	}
}

// TestWithDisplay checks DISPLAY is pinned and any inherited DISPLAY /
// WAYLAND_DISPLAY entries are stripped so the browser targets exactly one X
// display.
func TestWithDisplay(t *testing.T) {
	in := []string{"PATH=/bin", "DISPLAY=:0", "WAYLAND_DISPLAY=wayland-0", "HOME=/home/x"}
	out := withDisplay(in, ":99")
	if countPrefixArg(out, "DISPLAY=") != 1 {
		t.Errorf("want exactly one DISPLAY= entry, got %v", out)
	}
	if !containsArg(out, "DISPLAY=:99") {
		t.Errorf("want DISPLAY=:99, got %v", out)
	}
	if hasPrefixArg(out, "WAYLAND_DISPLAY=") {
		t.Errorf("WAYLAND_DISPLAY must be stripped, got %v", out)
	}
	if !containsArg(out, "PATH=/bin") || !containsArg(out, "HOME=/home/x") {
		t.Errorf("non-display env must be preserved, got %v", out)
	}
}

// TestCobrowse_StopWhenNotRunning verifies Stop is a safe no-op with no session
// and that calling it twice does not panic. The shutdown hook in cmd/work.go
// relies on this being safe to call unconditionally on every worker exit.
func TestCobrowse_StopWhenNotRunning(t *testing.T) {
	m := &CobrowseManager{driver: DriverAI, debugPort: DefaultCobrowseDebugPort}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop with no session should not error: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop should not error: %v", err)
	}
	if m.IsRunning() {
		t.Fatal("IsRunning should be false after Stop with no session")
	}
}

// TestCobrowse_ReaperDetectsExit verifies the load-bearing process-death fix:
// after the managed process exits on its own (simulating a Chromium crash),
// isRunningLocked flips to false instead of reporting the dead process as alive.
// Uses `sleep` as a stand-in process so the test needs no Chromium or display.
func TestCobrowse_ReaperDetectsExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep as a stand-in process")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	m := &CobrowseManager{driver: DriverAI, debugPort: DefaultCobrowseDebugPort}

	// Mirror the lifecycle Start() sets up: a running process plus the reaper
	// goroutine that closes exited when the process terminates.
	cmd := exec.Command("sleep", "0.2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	exited := make(chan struct{})
	m.cmd = cmd
	m.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	if !m.IsRunning() {
		t.Fatal("expected IsRunning=true while stand-in process is alive")
	}

	// Wait for the process to exit on its own; the reaper should close exited.
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("reaper did not observe process exit")
	}

	if m.IsRunning() {
		t.Fatal("expected IsRunning=false after the process exited (crash detection)")
	}
}

// TestCobrowse_StopKillsRunningProcess verifies Stop kills a live process,
// resets the driver to AI, and is safe to call again afterwards.
func TestCobrowse_StopKillsRunningProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX sleep as a stand-in process")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	m := &CobrowseManager{driver: DriverHuman, debugPort: DefaultCobrowseDebugPort}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	exited := make(chan struct{})
	m.cmd = cmd
	m.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	if !m.IsRunning() {
		t.Fatal("expected IsRunning=true before Stop")
	}
	_ = m.Stop()
	if m.IsRunning() {
		t.Fatal("expected IsRunning=false after Stop")
	}
	if m.driver != DriverAI {
		t.Fatalf("expected driver reset to ai after Stop, got %q", m.driver)
	}
	// Second Stop on the now-cleared manager must remain a safe no-op.
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop should not error: %v", err)
	}
}
