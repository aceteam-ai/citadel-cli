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

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", debugPort),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate",
		"--start-maximized",
	}
	if startURL != "" {
		args = append(args, startURL)
	}

	cmd := exec.Command(chrome, args...)
	// The browser must render on the SAME X display the node's VNC server
	// (x11vnc) exposes, so the existing /vnc/... stream shows it to the cockpit.
	// citadel may run under systemd with no DISPLAY in its env; default to :0
	// (the conventional x11vnc target) so headed Chromium lands on the streamed
	// display rather than failing to find one. detectLinuxDisplay() reads the
	// same DISPLAY var, so this keeps the browser and the VNC view in sync.
	env := os.Environ()
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		env = append(env, "DISPLAY=:0")
	}
	cmd.Env = env
	// Detach stdio; the browser is long-lived and headed on the node display.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return CobrowseStatus{}, fmt.Errorf("launch chromium: %w", err)
	}

	m.cmd = cmd
	m.driver = DriverAI
	m.debugPort = debugPort
	m.profile = profileDir
	m.chromePath = chrome
	m.startedAt = time.Now()

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
		return nil
	}
	err := m.cmd.Process.Kill()
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
	return err
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
