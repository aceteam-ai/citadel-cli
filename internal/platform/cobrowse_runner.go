// internal/platform/cobrowse_runner.go
//
// Browser runners for co-browse sessions (aceteam#13503 S1, citadel-cli#1548).
//
// The session manager (cobrowse_session.go), the CDP actions, the screencast
// bridge and the encrypted profile are shared across operating systems. What
// differs per OS is how a browser is found, launched, tied to the owning
// process's lifetime, and how a crashed owner's leftovers are reclaimed. That
// is the browserRunner interface below.
//
//   - xvfbRunner is the existing Linux path: a headed Chromium on its own
//     managed Xvfb display, with the /proc-verified PID sweep. Its behavior is
//     unchanged by the introduction of this interface.
//   - windowsHeadlessRunner (cobrowse_runner_windows.go) runs Chrome or Edge in
//     new headless mode inside a kill-on-close Job Object, so the browser never
//     takes over the user's screen and never outlives its owner.
//
// The runner's Stop half is the returned cobrowseProc.stop closure, which
// already owns per-launch teardown; there is no separate Stop method.
package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvCobrowseBrowser pins the browser executable a co-browse session launches.
// When set, discovery uses exactly this path and fails if it does not exist; it
// never silently falls back to another browser.
const EnvCobrowseBrowser = "CITADEL_COBROWSE_BROWSER"

// BrowserInfo identifies the browser a runner found.
type BrowserInfo struct {
	// Path is the absolute executable path (or a PATH-resolved command name).
	Path string `json:"path"`
	// Kind is "chrome", "edge" or "chromium".
	Kind string `json:"kind"`
}

// ErrNoBrowser reports that no supported browser was found. The wrapped message
// lists the places that were searched.
var ErrNoBrowser = errors.New("no supported browser found")

// ErrBrowserAutomationBlocked reports a browser that started but never opened
// its automation (remote debugging) endpoint while still running. On a managed
// machine this is almost always an organization policy that disables remote
// debugging.
var ErrBrowserAutomationBlocked = errors.New("this browser's organization policy blocks automation")

// browserRunner is the per-OS half of a co-browse session.
type browserRunner interface {
	// Name is a short identifier for status and doctor output.
	Name() string
	// Discover returns the browser this runner would launch, or a typed error
	// (ErrNoBrowser) naming what was searched.
	Discover() (BrowserInfo, error)
	// PrepareBaseDir is called before every launch with the manager's session
	// base dir. A runner that requires a trusted, private parent fails closed
	// here; the Linux runner keeps its historical behavior (no check at launch).
	PrepareBaseDir(baseDir string) error
	// Launch starts one isolated browser with profileDir and blocks until its CDP
	// endpoint is ready.
	Launch(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error)
	// SweepOrphans reclaims session dirs (and, where the OS lets it be done
	// safely, processes) left behind by an owner process that no longer exists.
	SweepOrphans(baseDir string)
}

// LaunchOptions are per-manager browser launch settings.
type LaunchOptions struct {
	// ProxyServer, when set, sends every browser connection (loopback
	// included, and no direct WebRTC UDP) through this HTTP proxy, so a
	// destination policy is enforced where the browser connects, not only on
	// the URL the agent asked for. Local mode sets it (aceteam#13503).
	ProxyServer string
	// Quiet turns off the browser's own background networking and component
	// updates, so a session only makes the connections its pages need.
	Quiet bool
	// Detach asks for the browser and Xvfb to run in their own systemd scopes so
	// they outlive a service restart. Only the node-wide manager of the managed
	// worker sets it; everything else launches directly.
	Detach bool
}

// devToolsActivePortFile is where a browser launched with
// --remote-debugging-port=0 writes its chosen port and browser id.
const devToolsActivePortFile = "DevToolsActivePort"

// errCDPImpostor reports a debug endpoint whose browser id does not match the
// one our browser wrote to its own profile dir.
var errCDPImpostor = errors.New("debug endpoint is not the browser this session launched")

// parseDevToolsActivePort reads "<port>\n/devtools/browser/<id>\n".
func parseDevToolsActivePort(data []byte) (int, string, error) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 {
		return 0, "", errors.New("incomplete DevToolsActivePort")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 || port > 65535 {
		return 0, "", fmt.Errorf("bad DevToolsActivePort port %q", lines[0])
	}
	path := strings.TrimSpace(lines[1])
	const prefix = "/devtools/browser/"
	if !strings.HasPrefix(path, prefix) || len(path) == len(prefix) {
		return 0, "", fmt.Errorf("bad DevToolsActivePort path %q", path)
	}
	return port, strings.TrimPrefix(path, prefix), nil
}

// waitForDevToolsActivePort waits for the browser to report its debug port.
func waitForDevToolsActivePort(profileDir string, timeout time.Duration, exited <-chan struct{}) (int, string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error = errors.New("DevToolsActivePort not written")
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(profileDir, devToolsActivePortFile)); err == nil {
			port, id, perr := parseDevToolsActivePort(data)
			if perr == nil {
				return port, id, nil
			}
			lastErr = perr
		}
		select {
		case <-exited:
			return 0, "", fmt.Errorf("browser exited before reporting its debug port: %v", lastErr)
		case <-time.After(150 * time.Millisecond):
		}
	}
	return 0, "", lastErr
}

// verifyCDPBrowserID checks the endpoint on port is the browser whose id was
// read from its own profile dir.
func verifyCDPBrowserID(port int, browserID string) error {
	if browserID == "" {
		return errCDPImpostor
	}
	got, err := fetchCDPBrowserID(port)
	if err != nil {
		return err
	}
	if got != browserID {
		return errCDPImpostor
	}
	return nil
}

// fetchCDPBrowserID reads the browser id the loopback endpoint on port reports
// (bounded client, no redirects).
func fetchCDPBrowserID(port int) (string, error) {
	resp, err := cdpHTTPClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&v); err != nil {
		return "", err
	}
	const marker = "/devtools/browser/"
	i := strings.LastIndex(v.WS, marker)
	if i < 0 || i+len(marker) == len(v.WS) {
		return "", errors.New("endpoint reports no browser id")
	}
	return v.WS[i+len(marker):], nil
}

// activeBrowserRunner is the runner for this OS. A package var so tests can
// substitute one; production sets it in cobrowse_runner_{windows,other}.go.
var activeBrowserRunner = newDefaultBrowserRunner()

// DiscoverCobrowseBrowser reports the browser co-browse sessions would launch on
// this machine, without launching it.
func DiscoverCobrowseBrowser() (BrowserInfo, error) { return activeBrowserRunner.Discover() }

// CobrowseRunnerName reports which runner this OS uses ("xvfb" or "headless").
func CobrowseRunnerName() string { return activeBrowserRunner.Name() }

// browserKindForPath classifies an executable path by its base name.
func browserKindForPath(path string) string {
	base := lowerBase(path)
	switch base {
	case "msedge.exe", "msedge", "microsoft-edge", "microsoft-edge-stable":
		return "edge"
	case "chrome.exe", "chrome", "google-chrome", "google-chrome-stable":
		return "chrome"
	default:
		return "chromium"
	}
}

// lowerBase returns the lower-cased final path element, accepting either
// separator so Windows-style paths classify correctly on any host.
func lowerBase(path string) string {
	end := len(path)
	start := 0
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '\\' || path[i] == '/' {
			start = i + 1
			break
		}
	}
	b := []byte(path[start:end])
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// xvfbRunner is the historical Linux co-browse launcher: headed Chromium on a
// dedicated managed Xvfb display. It is a thin wrapper so the Linux path keeps
// exactly its pre-interface behavior.
type xvfbRunner struct{}

func (xvfbRunner) Name() string { return "xvfb" }

func (xvfbRunner) Discover() (BrowserInfo, error) {
	path, err := findChromium()
	if err != nil {
		return BrowserInfo{}, fmt.Errorf("%w: %v", ErrNoBrowser, err)
	}
	return BrowserInfo{Path: path, Kind: browserKindForPath(path)}, nil
}

// PrepareBaseDir is a no-op: on Linux the trust check runs in the sweep, which
// refuses to read pidfiles from an untrusted parent (unchanged behavior).
func (xvfbRunner) PrepareBaseDir(string) error { return nil }

func (xvfbRunner) Launch(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error) {
	return launchXvfbCobrowseProc(profileDir, startURL, lo)
}

func (xvfbRunner) SweepOrphans(baseDir string) { sweepXvfbSessions(baseDir) }

// launchXvfbCobrowseProc is the real Linux browser launcher: a headless-by-default
// Chromium on a fresh dedicated Xvfb display with an isolated profile and CDP port.
// Mirrors MeetingBrowser.Start's teardown-on-partial-failure, but the profile is
// throwaway so a failed launch removal is handled by the caller.
func launchXvfbCobrowseProc(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error) {
	if !lo.Detach {
		return launchXvfbAttempt(profileDir, startURL, lo, nil)
	}
	plan, reason := planCobrowseScope()
	if plan == nil {
		logScopeFailOpen(reason)
		return launchXvfbAttempt(profileDir, startURL, lo, nil)
	}
	proc, err := launchXvfbAttempt(profileDir, startURL, lo, plan)
	if err != nil {
		// Fail open like tmux: a scope the manager refuses must not cost the
		// session. Retry once as a direct child (it then dies with a restart).
		scopeBroken.Store(true)
		logScopeFailOpen("scoped launch failed: " + err.Error())
		return launchXvfbAttempt(profileDir, startURL, lo, nil)
	}
	return proc, nil
}

// launchXvfbAttempt starts one session, in transient scopes when plan is set.
func launchXvfbAttempt(profileDir, startURL string, lo LaunchOptions, plan *scopePlan) (*cobrowseProc, error) {
	chrome, err := findChromium()
	if err != nil {
		return nil, err
	}

	debugPort, err := findFreeDebugPort()
	if err != nil {
		return nil, err
	}

	sessionID := filepath.Base(profileDir)
	if plan != nil && !sessionIDPattern.MatchString(sessionID) {
		plan = nil
	}
	var xvfbUnit, browserUnit string
	var wrapXvfb func([]string) []string
	if plan != nil {
		xvfbUnit, browserUnit = plan.unit(sessionID, "xvfb"), plan.unit(sessionID, "browser")
		wrapXvfb = func(a []string) []string { return plan.wrap(xvfbUnit, a) }
	}

	// startManagedXvfb serializes display allocation internally, so concurrent
	// launches never collide on one virtual display.
	xvfb, display, err := startManagedXvfbWrapped(xvfbResolution(), wrapXvfb)
	if err != nil {
		return nil, err
	}

	// A managed Xvfb has no GPU, so force software rendering (matches the sibling
	// launchers). stealthEnabled() keeps parity with the other browser launches.
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:   debugPort,
		profileDir:  profileDir,
		startURL:    startURL,
		stealth:     stealthEnabled(),
		userAgent:   os.Getenv(EnvCobrowseUserAgent),
		softwareGL:  true,
		proxyServer: lo.ProxyServer,
		quiet:       lo.Quiet,
	})

	argv := append([]string{chrome}, args...)
	if plan != nil {
		argv = plan.wrap(browserUnit, argv)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = withDisplay(os.Environ(), display)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		if xvfb.Process != nil {
			_ = xvfb.Process.Kill()
		}
		return nil, fmt.Errorf("launch browser: %w", err)
	}

	// Reap each child so a crash is observable (exited) and no zombie is left. stop
	// signals the kill and waits on these channels; it never calls Wait directly.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	xvfbExited := make(chan struct{})
	go func() {
		_ = xvfb.Wait()
		close(xvfbExited)
	}()

	proc := &cobrowseProc{
		debugPort:  debugPort,
		display:    display,
		browser:    browserKindForPath(chrome),
		browserPID: pidOf(cmd),
		xvfbPID:    pidOf(xvfb),
		exited:     exited,
	}
	var bStart, xStart uint64
	if plan != nil {
		proc.browserScope, proc.xvfbScope, proc.scopeUser = browserUnit, xvfbUnit, plan.user
		bStart, _, _ = procIdentity(proc.browserPID)
		xStart, _, _ = procIdentity(proc.xvfbPID)
	}
	proc.stop = func() error {
		// Browser scope first, then Xvfb, each only while the recorded process
		// still lives in it; the handle kill below is the backstop.
		var serr error
		if plan != nil {
			if err := stopScope(plan.user, browserUnit, proc.browserPID, bStart); err != nil {
				serr = err
			}
			if err := stopScope(plan.user, xvfbUnit, proc.xvfbPID, xStart); err != nil && serr == nil {
				serr = err
			}
		}
		if err := stopBrowserProc(cmd, exited, xvfb, xvfbExited); err != nil {
			return err
		}
		return serr
	}

	if err := waitForCDPReady(debugPort, 20*time.Second); err != nil {
		// Best-effort teardown so a browser that launched but never exposed CDP does
		// not leak a process or display.
		_ = proc.stop()
		return nil, fmt.Errorf("browser launched but CDP not ready: %w", err)
	}
	return proc, nil
}

// deheadlessUserAgent turns a headless browser's default user agent into the
// one the same browser sends in a normal window: new headless mode reports
// "HeadlessChrome/<v>" where a headed browser reports "Chrome/<v>". The rest
// (OS, Edge's "Edg/<v>" suffix, versions) is kept exactly, so nothing is
// hardcoded and the UA always matches the installed binary.
func deheadlessUserAgent(ua string) string {
	return strings.TrimSpace(strings.ReplaceAll(ua, "HeadlessChrome/", "Chrome/"))
}

// browserDefaultUserAgent reads the "User-Agent" field of a running browser's
// loopback /json/version endpoint.
func browserDefaultUserAgent(debugPort int) (string, error) {
	resp, err := cdpHTTPClient.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", debugPort))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/json/version: %s", resp.Status)
	}
	var v struct {
		UserAgent string `json:"User-Agent"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&v); err != nil {
		return "", err
	}
	return v.UserAgent, nil
}

// removeAllRetry removes dir, retrying briefly. On Windows a just-terminated
// browser can hold profile files open for a moment after its process exits,
// which makes a single RemoveAll fail transiently.
func removeAllRetry(dir string, attempts int, delay time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return err
}
