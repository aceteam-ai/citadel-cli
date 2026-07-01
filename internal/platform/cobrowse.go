// internal/platform/cobrowse.go
//
// Co-browse session manager: a single, long-lived headed Chromium process on
// the node, driven by the AI agent over the Chrome DevTools Protocol (CDP) and
// handed off to a human for login / 2FA / manual interaction (issue #4079).
//
// Design (mirrors the CloakBrowser prior art td_serve.py + td_step.py):
//   - One persistent headed Chromium per node, launched with
//     --remote-debugging-port and a persistent --user-data-dir so cookies and
//     logins survive across jobs. The browser lives BETWEEN jobs; each MCP call
//     is a short Redis job that connects to the running browser, performs one
//     action, and disconnects -- it never owns the browser lifecycle.
//   - The AI steers over CDP (Page.navigate / Page.captureScreenshot). We talk
//     raw CDP over the DevTools WebSocket using gorilla/websocket (already a
//     dependency) rather than adding a heavyweight headless-control library, so
//     the node binary stays small and `go build`/`go vet` stay clean.
//   - A driver-state machine enforces human-in-the-loop handoff: when the
//     driver is the human, AI-issued navigate/screenshot calls are REFUSED so
//     the agent cannot stomp on a human mid-2FA. cobrowse_resume returns
//     control to the agent.
//
// Live view is NOT implemented here. The browser runs headed on the node's
// display, so the EXISTING VNC WebSocket bridge (`/vnc/...` -> websockify, see
// cmd/serve.go / cmd/work.go) already streams the browser to the cockpit. No
// new streaming transport is added; co-browse reuses the desktop stream.
package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultCobrowseDebugPort is the CDP remote-debugging port the managed
// Chromium listens on. Loopback-only; reached over the mesh via the node's
// existing tunnels, never bound to a public interface.
const DefaultCobrowseDebugPort = 9222

// EnvCobrowseStealth toggles the anti-bot-detection ("stealth") launch flags.
// Stealth is ON by default for co-browse; set this env var to "0" / "false" /
// "off" to launch a plain automation-flavored Chromium instead (useful for
// debugging or for parity with the pre-stealth behavior).
const EnvCobrowseStealth = "CITADEL_COBROWSE_STEALTH"

// EnvCobrowseUserAgent optionally overrides the browser User-Agent. By default
// we leave the UA UNSET so real headed Chrome reports its own (correct, current)
// UA -- a hardcoded UA pins a Chrome version that drifts out of sync with the
// actual binary, and that mismatch is itself a bot-detection signal. Only set
// this when you deliberately want to spoof a specific UA.
const EnvCobrowseUserAgent = "CITADEL_COBROWSE_USER_AGENT"

// EnvCobrowseDisplay, when set, pins the X display the co-browse browser renders
// on (e.g. ":0" to share the node's real desktop for a watch-along, or ":99" for
// a pre-existing virtual display). When UNSET (the default), the manager starts
// its OWN dedicated Xvfb virtual display. The dedicated display is the better
// default on two axes:
//   - Headless nodes (GPU/compute boxes with no monitor and no :0) can run
//     co-browse at all -- a hardcoded :0 fails there.
//   - Privacy/isolation: the live VNC view shows ONLY the co-browse browser, not
//     whatever else is open on the operator's real desktop. Co-browse is meant to
//     be a throwaway screen the agent drives, not a window onto the user's machine.
//
// Set this to ":0" (or the operator's real DISPLAY) to opt back into the legacy
// shared-desktop behavior.
const EnvCobrowseDisplay = "CITADEL_COBROWSE_DISPLAY"

// EnvCobrowseResolution overrides the managed Xvfb virtual-display geometry as
// WIDTHxHEIGHTxDEPTH. Defaults to defaultXvfbResolution.
const EnvCobrowseResolution = "CITADEL_COBROWSE_RESOLUTION"

// defaultXvfbResolution is the managed virtual-display geometry when
// EnvCobrowseResolution is unset.
const defaultXvfbResolution = "1920x1080x24"

// cobrowseLaunchOptions is the full input to buildChromeArgs. Keeping it a plain
// struct (rather than an interface/registry) is deliberate: a future full
// CloakBrowser launcher swaps in by constructing different options or by calling
// a different builder, without any plugin abstraction to maintain here.
type cobrowseLaunchOptions struct {
	debugPort  int
	profileDir string
	startURL   string
	// stealth enables the anti-bot-detection launch flags (see buildChromeArgs).
	stealth bool
	// lang is the UI/Accept-Language locale (e.g. "en-US"). Defaults applied by
	// buildChromeArgs when empty.
	lang string
	// userAgent, when non-empty, overrides the browser User-Agent. Empty means
	// "use real Chrome's own UA" (recommended -- see EnvCobrowseUserAgent).
	userAgent string
	// softwareGL forces software rendering (--disable-gpu). Set when the browser
	// runs on a dedicated Xvfb virtual display, which has no GPU/GLX -- without it
	// headed Chromium spawns a GPU process that fails to initialize on Xvfb and
	// logs churn. Left false when sharing a real display that may have a GPU.
	softwareGL bool
}

// stealthEnabled resolves whether stealth launch flags should be applied,
// reading EnvCobrowseStealth. Stealth is ON unless explicitly disabled.
func stealthEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvCobrowseStealth))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// buildChromeArgs constructs the Chromium command-line for a co-browse launch.
// It is a pure function (no I/O, no globals beyond its inputs) so the flag set
// is unit-testable without launching a browser or touching a display.
//
// Stealth (anti-bot-detection) notes -- this is the citadel-side, launch-flag
// layer of the team's stealth-Chromium approach (CloakBrowser), which is the
// intended long-term replacement for this plain launch:
//   - --disable-blink-features=AutomationControlled removes the headless/automation
//     Blink feature so navigator.webdriver is not forced true. This is the single
//     most load-bearing stealth flag.
//   - We deliberately NEVER pass --enable-automation. Launching raw via exec means
//     Chrome does not add the automation switches or the "Chrome is being controlled
//     by automated test software" infobar unless we ask for them -- so simply not
//     emitting that flag satisfies the "no infobar / exclude enable-automation"
//     requirement. (--exclude-switches / excludeSwitches is a chromedriver
//     capability, not a Chrome CLI flag, so there is intentionally no such arg.
//     --disable-infobars was removed from modern Chrome and is intentionally
//     omitted too.)
//   - --lang pins a realistic locale.
//   - --user-data-dir gives a persistent stealth profile so cookies, history, and
//     the resulting fingerprint stay consistent across jobs.
//
// What stealth flags CANNOT do (and therefore needs the full CloakBrowser
// wrapper, tracked as follow-up): CDP-level
// Page.addScriptToEvaluateOnNewDocument fingerprint patches (navigator.webdriver /
// plugins / languages, window.chrome.runtime, permissions.query, WebGL
// vendor/renderer, canvas/audio noise), per-identity consistent fingerprints,
// and proxy/TLS (JA3) alignment.
func buildChromeArgs(opts cobrowseLaunchOptions) []string {
	// disableFeatures is built as a single slice and emitted as ONE
	// --disable-features flag. Chrome only honors the last --disable-features
	// occurrence, so appending a second flag would silently drop earlier values
	// (e.g. Translate). Future stealth additions go in this slice, not a new flag.
	disableFeatures := []string{"Translate"}

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", opts.debugPort),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + opts.profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--start-maximized",
	}

	if opts.stealth {
		// Core anti-automation signal: drop the AutomationControlled Blink
		// feature so navigator.webdriver is not forced true.
		args = append(args, "--disable-blink-features=AutomationControlled")
		lang := opts.lang
		if lang == "" {
			lang = "en-US"
		}
		args = append(args, "--lang="+lang)
		if opts.userAgent != "" {
			args = append(args, "--user-agent="+opts.userAgent)
		}
	}

	// Emit the merged disable-features exactly once (see note above).
	args = append(args, "--disable-features="+strings.Join(disableFeatures, ","))

	if opts.softwareGL {
		// Dedicated Xvfb has no GPU/GLX; force the software path so the GPU
		// process does not fail to initialize. Page.captureScreenshot still works
		// via the software compositor.
		args = append(args, "--disable-gpu")
	}

	if opts.startURL != "" {
		args = append(args, opts.startURL)
	}
	return args
}

// CobrowseDriver identifies who currently controls the session.
type CobrowseDriver string

const (
	// DriverAI means the agent may issue navigate/screenshot actions.
	DriverAI CobrowseDriver = "ai"
	// DriverHuman means control is handed off to the human (login / 2FA /
	// manual interaction). AI-issued actions are refused until resume.
	DriverHuman CobrowseDriver = "human"
)

// ErrHandedOff is returned by AI-driven actions while the session is handed off
// to the human. Callers (the job handler, then the backend MCP tool) surface
// this so the agent knows to call cobrowse_resume rather than fighting the human.
var ErrHandedOff = fmt.Errorf("session handed off to human; call cobrowse_resume to return control to the agent")

// ErrNotStarted is returned when an action is requested before cobrowse_start.
var ErrNotStarted = fmt.Errorf("no co-browse session; call cobrowse_start first")

// CobrowseStatus is the queryable state of the session.
type CobrowseStatus struct {
	Running   bool           `json:"running"`
	Driver    CobrowseDriver `json:"driver"`
	URL       string         `json:"url"`
	DebugPort int            `json:"debug_port"`
	Profile   string         `json:"profile"`
	Display   string         `json:"display,omitempty"`
	StartedAt string         `json:"started_at,omitempty"`
}

// CobrowseManager owns the single managed browser process and its driver state.
// It is safe for concurrent use; each Redis job touches it under the mutex.
//
// Concurrency note: every action holds the manager mutex across its CDP network
// I/O (HTTP probe + WebSocket round-trip). This serializes co-browse jobs on the
// node, which is intentional -- co-browse is single-session-per-node, so there is
// never more than one browser to drive and serializing keeps the driver-state
// machine race-free. The bounded cdpHTTPClient / per-call WebSocket deadlines
// ensure a hung DevTools endpoint cannot hold the mutex indefinitely.
type CobrowseManager struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	driver     CobrowseDriver
	debugPort  int
	profile    string
	startedAt  time.Time
	chromePath string
	// exited is closed by the reaper goroutine when the managed browser process
	// terminates (whether killed by Stop or crashed on its own). isRunningLocked
	// consults it so a dead browser is not reported as running.
	exited chan struct{}
	// display is the X display the browser renders on (e.g. ":99" for the managed
	// virtual display, or an operator-pinned ":0"). Reported in status.
	display string
	// xvfb is the dedicated Xvfb virtual-display process when one is managed (nil
	// when an operator-pinned display is used). Owned by this manager: started in
	// Start(), reaped by xvfbExited, and killed by Stop() AFTER the browser.
	xvfb       *exec.Cmd
	xvfbExited chan struct{}
}

var (
	cobrowseManager     *CobrowseManager
	cobrowseManagerOnce sync.Once
)

// GetCobrowseManager returns the process-wide co-browse manager singleton,
// mirroring GetVNCManager(). The browser persists across jobs in this one
// manager; that is what makes "human logs in once, AI keeps steering" work.
func GetCobrowseManager() *CobrowseManager {
	cobrowseManagerOnce.Do(func() {
		cobrowseManager = &CobrowseManager{
			driver:    DriverAI,
			debugPort: DefaultCobrowseDebugPort,
		}
	})
	return cobrowseManager
}

// findChromium returns the first available Chromium/Chrome binary, reusing the
// same preference order as platform.OpenURL.
func findChromium() (string, error) {
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if isCommandAvailable(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("no Chromium/Chrome binary found (install google-chrome or chromium)")
}

// displayMode is how the co-browse browser obtains an X display.
type displayMode int

const (
	// displayManaged starts a dedicated Xvfb virtual display (the default).
	// Headless-safe and isolates the session from the node's real desktop.
	displayManaged displayMode = iota
	// displayExplicit uses an operator-pinned DISPLAY verbatim (EnvCobrowseDisplay).
	displayExplicit
)

// resolveCobrowseDisplay decides how the browser gets a display from the
// environment alone. Pure (no I/O) so the precedence is unit-testable without
// launching an X server: an explicit EnvCobrowseDisplay wins, otherwise a
// dedicated Xvfb is managed.
func resolveCobrowseDisplay() (displayMode, string) {
	if d := strings.TrimSpace(os.Getenv(EnvCobrowseDisplay)); d != "" {
		return displayExplicit, d
	}
	return displayManaged, ""
}

// xvfbResolution returns the managed virtual-display geometry, honoring
// EnvCobrowseResolution and falling back to defaultXvfbResolution.
func xvfbResolution() string {
	if r := strings.TrimSpace(os.Getenv(EnvCobrowseResolution)); r != "" {
		return r
	}
	return defaultXvfbResolution
}

// fileExists reports whether a path exists (used to detect live X sockets/locks).
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// findFreeDisplay returns the first display number >= 99 with no X socket or
// lock file, so the managed Xvfb never collides with an existing server -- the
// operator's real :0/:1, another node tool, or a prior co-browse Xvfb.
func findFreeDisplay() int {
	for n := 99; n < 99+128; n++ {
		if !fileExists(fmt.Sprintf("/tmp/.X11-unix/X%d", n)) &&
			!fileExists(fmt.Sprintf("/tmp/.X%d-lock", n)) {
			return n
		}
	}
	return 99
}

// startManagedXvfb launches a dedicated Xvfb virtual display and waits for its
// socket so the browser launch does not race startup. Returns the running
// command and the ":N" display string; the caller owns reaping (mirroring the
// Chromium reaper). A missing Xvfb binary yields an actionable error rather than
// a confusing downstream "CDP not ready" timeout.
func startManagedXvfb(resolution string) (*exec.Cmd, string, error) {
	if !isCommandAvailable("Xvfb") {
		return nil, "", fmt.Errorf(
			"Xvfb not found: install it (e.g. 'apt-get install xvfb') to run headless co-browse, "+
				"or set %s to an existing X display (e.g. :0)", EnvCobrowseDisplay)
	}
	n := findFreeDisplay()
	display := fmt.Sprintf(":%d", n)
	cmd := exec.Command("Xvfb", display, "-screen", "0", resolution, "-nolisten", "tcp")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("launch Xvfb on %s: %w", display, err)
	}
	sock := fmt.Sprintf("/tmp/.X11-unix/X%d", n)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fileExists(sock) {
			return cmd, display, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return nil, "", fmt.Errorf("Xvfb on %s did not become ready within 8s", display)
}

// withDisplay returns env with DISPLAY set to the chosen value and any inherited
// DISPLAY / WAYLAND_DISPLAY removed, so the headed browser targets exactly the
// intended X display (and never silently falls onto a Wayland socket).
func withDisplay(env []string, display string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "DISPLAY=") || strings.HasPrefix(kv, "WAYLAND_DISPLAY=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "DISPLAY="+display)
}

// IsRunning reports whether the managed browser process is alive.
func (m *CobrowseManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isRunningLocked()
}

func (m *CobrowseManager) isRunningLocked() bool {
	if m.cmd == nil || m.cmd.Process == nil {
		return false
	}
	// cmd.Process stays non-nil for the life of the struct even after the
	// browser dies, so consult the reaper's exited channel to detect a crash.
	select {
	case <-m.exited:
		return false
	default:
		return true
	}
}

// Start launches the headed Chromium with a persistent profile and CDP port.
// If a session is already running it is a no-op (idempotent) and the existing
// session is reused -- co-browse is single-session-per-node by design;
// multi-session is left as future work. startURL may be empty.
func (m *CobrowseManager) Start(profileDir, startURL string, debugPort int) (CobrowseStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.isRunningLocked() {
		// Reuse the existing browser; optionally navigate if a URL was given
		// and the AI holds the driver token.
		if startURL != "" && m.driver == DriverAI {
			_ = m.navigateLocked(startURL)
		}
		return m.statusLocked(), nil
	}

	if debugPort <= 0 {
		debugPort = DefaultCobrowseDebugPort
	}
	if profileDir == "" {
		profileDir = filepath.Join(os.TempDir(), "citadel-cobrowse-profile")
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return CobrowseStatus{}, fmt.Errorf("create profile dir: %w", err)
	}

	chrome, err := findChromium()
	if err != nil {
		return CobrowseStatus{}, err
	}

	// Clear any Xvfb left over from a prior launch that started the virtual
	// display but then failed (e.g. CDP never came up) and whose browser has
	// since died. We are not running (checked above), so any lingering managed
	// Xvfb is stale and must be reaped before starting a fresh one, or it leaks.
	m.teardownXvfbLocked()

	// Reclaim a stale CDP port before launching (issue #396). We reach here only
	// when NOT running, so anything currently listening on the debug port is an
	// ORPHAN Chromium from a prior worker that was SIGKILLed / crashed before its
	// Stop() ran. Without this, waitForCDP below would connect to that ghost and
	// this manager would report running:true for a browser it never launched --
	// while m.cmd stays nil, so every later action fails ErrNotStarted. Killing
	// the port owner guarantees the browser we launch is the one m.cmd tracks.
	if reclaimed, rerr := reclaimStalePort(debugPort); rerr != nil {
		return CobrowseStatus{}, fmt.Errorf("stale co-browse browser on CDP port %d: %w", debugPort, rerr)
	} else if reclaimed {
		// The killed orphan may still be releasing the socket; a short settle
		// avoids the fresh Chromium racing a closing listener on the port.
		time.Sleep(300 * time.Millisecond)
	}

	// Resolve the X display the browser renders on. Default: a dedicated Xvfb
	// virtual display, which (a) works on headless nodes with no :0 and (b)
	// isolates the session from the operator's real desktop. Opt into a shared
	// real display by setting EnvCobrowseDisplay (e.g. ":0"). A managed Xvfb runs
	// without a GPU, so force software rendering for that case.
	mode, explicitDisplay := resolveCobrowseDisplay()
	var xvfb *exec.Cmd
	display := explicitDisplay
	softwareGL := false
	if mode == displayManaged {
		x, d, derr := startManagedXvfb(xvfbResolution())
		if derr != nil {
			return CobrowseStatus{}, derr
		}
		xvfb, display, softwareGL = x, d, true
	}

	// Stealth (anti-bot-detection) is ON by default for co-browse; this is the
	// first citadel-side step toward the full CloakBrowser launch replacing this
	// plain Chromium launch. Disable via EnvCobrowseStealth for plain behavior.
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:  debugPort,
		profileDir: profileDir,
		startURL:   startURL,
		stealth:    stealthEnabled(),
		userAgent:  os.Getenv(EnvCobrowseUserAgent),
		softwareGL: softwareGL,
	})

	cmd := exec.Command(chrome, args...)
	// Pin the browser to the resolved display (managed Xvfb or operator-set), so
	// the live VNC/desktop view in the cockpit shows exactly this browser. The
	// managed virtual display means that view is the co-browse browser ALONE,
	// never the operator's other windows.
	cmd.Env = withDisplay(os.Environ(), display)
	// Detach stdio; the browser is long-lived and headed on the node display.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		// Tear down the just-started Xvfb so a failed browser launch does not leak
		// a virtual display.
		if xvfb != nil && xvfb.Process != nil {
			_ = xvfb.Process.Kill()
		}
		return CobrowseStatus{}, fmt.Errorf("launch chromium: %w", err)
	}

	m.cmd = cmd
	m.driver = DriverAI
	m.debugPort = debugPort
	m.profile = profileDir
	m.chromePath = chrome
	m.startedAt = time.Now()
	m.display = display

	// Own the managed Xvfb lifecycle alongside the browser: reap it so a crash is
	// observable and Stop() can tear it down without double-Wait.
	m.xvfb = xvfb
	if xvfb != nil {
		xvfbExited := make(chan struct{})
		m.xvfbExited = xvfbExited
		go func() {
			_ = xvfb.Wait()
			close(xvfbExited)
		}()
	}

	// Reaper: own the single Wait() for this process. When Chromium exits (killed
	// by Stop or crashed on its own), close exited so isRunningLocked stops
	// reporting it as running and the OS process is reaped (no zombie). Stop must
	// NOT call Wait() itself -- that would double-Wait and race this goroutine.
	exited := make(chan struct{})
	m.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// Wait for the CDP endpoint to come up so the first navigate/screenshot
	// does not race the browser launch.
	if err := m.waitForCDP(debugPort, 15*time.Second); err != nil {
		return m.statusLocked(), fmt.Errorf("browser launched but CDP not ready: %w", err)
	}

	return m.statusLocked(), nil
}

// Stop terminates the managed browser. Safe to call when not running and safe
// to call twice. The reaper goroutine owns Wait(); Stop only signals the kill
// and waits for the reaper to confirm the process is gone before clearing state.
func (m *CobrowseManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil || m.cmd.Process == nil {
		m.cmd = nil
		m.exited = nil
		m.driver = DriverAI
		m.teardownXvfbLocked()
		return nil
	}
	err := m.cmd.Process.Kill()
	// A browser that already exited on its own (crash, or the reaper saw it die)
	// is the goal state, not a failure. Swallow "process already finished" so
	// shutdown does not log a spurious "co-browse browser stop" warning and so
	// teardown always proceeds to reap the Xvfb (issue #396).
	if isProcessGoneErr(err) {
		err = nil
	}
	if m.exited != nil {
		// Wait for the reaper to reap the process (bounded) so we never leave a
		// zombie. The reaper closes exited after cmd.Wait() returns.
		select {
		case <-m.exited:
		case <-time.After(5 * time.Second):
		}
	}
	m.cmd = nil
	m.exited = nil
	m.driver = DriverAI
	// Tear down the virtual display AFTER the browser so the browser is never
	// left without a display mid-shutdown.
	m.teardownXvfbLocked()
	return err
}

// teardownXvfbLocked kills the managed Xvfb (if any) and waits, bounded, for the
// reaper to confirm it exited so no zombie or orphaned virtual display is left.
// No-op when an operator-pinned display was used (m.xvfb == nil). Caller holds m.mu.
func (m *CobrowseManager) teardownXvfbLocked() {
	if m.xvfb != nil && m.xvfb.Process != nil {
		_ = m.xvfb.Process.Kill()
		if m.xvfbExited != nil {
			select {
			case <-m.xvfbExited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	m.xvfb = nil
	m.xvfbExited = nil
	m.display = ""
}

// Handoff transfers control to the human. Idempotent.
func (m *CobrowseManager) Handoff() (CobrowseStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isRunningLocked() {
		return CobrowseStatus{}, ErrNotStarted
	}
	m.driver = DriverHuman
	return m.statusLocked(), nil
}

// Resume returns control to the AI agent. Idempotent.
func (m *CobrowseManager) Resume() (CobrowseStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isRunningLocked() {
		return CobrowseStatus{}, ErrNotStarted
	}
	m.driver = DriverAI
	return m.statusLocked(), nil
}

// Status returns the current queryable session state.
func (m *CobrowseManager) Status() CobrowseStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked()
}

func (m *CobrowseManager) statusLocked() CobrowseStatus {
	st := CobrowseStatus{
		Running:   m.isRunningLocked(),
		Driver:    m.driver,
		DebugPort: m.debugPort,
		Profile:   m.profile,
		Display:   m.display,
	}
	if !m.startedAt.IsZero() {
		st.StartedAt = m.startedAt.UTC().Format(time.RFC3339)
	}
	if st.Running {
		if url, err := m.currentURL(m.debugPort); err == nil {
			st.URL = url
		}
	}
	return st
}

// Navigate drives the browser to a URL. Refused while handed off to the human.
func (m *CobrowseManager) Navigate(url string) (CobrowseStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isRunningLocked() {
		return CobrowseStatus{}, ErrNotStarted
	}
	if m.driver == DriverHuman {
		return m.statusLocked(), ErrHandedOff
	}
	if err := m.navigateLocked(url); err != nil {
		return m.statusLocked(), err
	}
	return m.statusLocked(), nil
}

// Screenshot captures the current viewport as base64 PNG. Refused while handed
// off to the human (the live VNC stream is the human's view during handoff).
func (m *CobrowseManager) Screenshot() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isRunningLocked() {
		return "", ErrNotStarted
	}
	if m.driver == DriverHuman {
		return "", ErrHandedOff
	}
	return m.screenshotLocked(m.debugPort)
}

// ---------------------------------------------------------------------------
// Raw CDP plumbing (gorilla/websocket). One short-lived WebSocket per action,
// mirroring td_step.py's connect-act-disconnect model.
// ---------------------------------------------------------------------------

type cdpTarget struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpHTTPClient bounds the CDP HTTP probe so a hung DevTools endpoint cannot
// block the manager mutex (and thus every subsequent co-browse job) forever.
var cdpHTTPClient = &http.Client{Timeout: 5 * time.Second}

// pickTarget returns the first "page" target's WebSocket URL from /json.
func pickTarget(debugPort int) (cdpTarget, error) {
	resp, err := cdpHTTPClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json", debugPort))
	if err != nil {
		return cdpTarget{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return cdpTarget{}, err
	}
	var targets []cdpTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return cdpTarget{}, err
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t, nil
		}
	}
	return cdpTarget{}, fmt.Errorf("no page target found on CDP port %d", debugPort)
}

// cdpCommand opens a WebSocket to the page target, sends one CDP method, and
// returns the "result" object. id is fixed to 1 since the socket is per-call.
func cdpCommand(debugPort int, method string, params map[string]any) (map[string]any, error) {
	target, err := pickTarget(debugPort)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("CDP dial: %w", err)
	}
	defer conn.Close()

	if params == nil {
		params = map[string]any{}
	}
	req := map[string]any{"id": 1, "method": method, "params": params}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("CDP write: %w", err)
	}

	// Read until we see the response with id==1 (skip async events).
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			return nil, fmt.Errorf("CDP read: %w", err)
		}
		idv, ok := msg["id"]
		if !ok {
			continue // event, not our response
		}
		if fmt.Sprint(idv) != "1" {
			continue
		}
		if e, ok := msg["error"]; ok {
			return nil, fmt.Errorf("CDP error: %v", e)
		}
		if res, ok := msg["result"].(map[string]any); ok {
			return res, nil
		}
		return map[string]any{}, nil
	}
}

func (m *CobrowseManager) navigateLocked(url string) error {
	_, err := cdpCommand(m.debugPort, "Page.navigate", map[string]any{"url": url})
	return err
}

func (m *CobrowseManager) screenshotLocked(debugPort int) (string, error) {
	res, err := cdpCommand(debugPort, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return "", err
	}
	data, _ := res["data"].(string)
	if data == "" {
		return "", fmt.Errorf("empty screenshot data from CDP")
	}
	// CDP already returns base64; validate it decodes so we fail loudly on junk.
	if _, derr := base64.StdEncoding.DecodeString(data); derr != nil {
		return "", fmt.Errorf("screenshot not valid base64: %w", derr)
	}
	return data, nil
}

func (m *CobrowseManager) currentURL(debugPort int) (string, error) {
	target, err := pickTarget(debugPort)
	if err != nil {
		return "", err
	}
	return target.URL, nil
}

// waitForCDP polls the CDP HTTP endpoint until a page target appears or the
// timeout elapses.
func (m *CobrowseManager) waitForCDP(debugPort int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := pickTarget(debugPort); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	if lastErr != nil && strings.Contains(lastErr.Error(), "no page target") {
		// Endpoint is up but no page yet -- treat as ready enough.
		return nil
	}
	return fmt.Errorf("CDP endpoint not ready after %s: %v", timeout, lastErr)
}
