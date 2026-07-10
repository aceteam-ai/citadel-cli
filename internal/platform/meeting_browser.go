// internal/platform/meeting_browser.go
//
// Meeting-bot browser: a headed Chromium the notetaker drives over CDP to join a
// video call, launched so its audio routes into a per-meeting PulseAudio null
// sink for capture (issue #5098, epic #5097 — the sovereign auto-join notetaker).
//
// This is a deliberate SIBLING of CobrowseManager, not a reuse of it. The
// co-browse manager is a process-wide singleton owning ONE long-lived browser
// that a human logs into and the AI keeps steering. A meeting bot needs the
// opposite: a short-lived, disposable browser per meeting, isolated from the
// co-browse session so the two never fight over one Chromium (Chrome locks a
// --user-data-dir to a single process, so they cannot share a profile anyway).
// MeetingBrowser therefore owns its OWN Xvfb display, CDP debug port, and
// throwaway profile dir, and reuses only the package-level, side-effect-free
// launch helpers (buildChromeArgs, startManagedXvfb, withDisplay, findChromium,
// pickTarget, cdpCommand) so there is no duplicated browser-launch logic.
package platform

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ChromiumAvailable reports whether a Chromium/Chrome binary is on PATH. Exported
// so capability detection can gate the `meeting` tag on a launchable browser
// without reaching into this package's unexported findChromium.
func ChromiumAvailable() bool {
	_, err := findChromium()
	return err == nil
}

// XvfbAvailable reports whether the Xvfb binary is on PATH. The meeting browser
// always runs on a dedicated virtual display (meeting nodes are typically
// headless), so Xvfb is a hard dependency of the `meeting` capability.
func XvfbAvailable() bool {
	return isCommandAvailable("Xvfb")
}

// AudioStackAvailable is the exported form of audioStackAvailable so capability
// detection (a different package) can gate the `meeting` tag on a working
// PulseAudio + ffmpeg + pactl stack.
func AudioStackAvailable() bool {
	return audioStackAvailable()
}

// findFreeDebugPort asks the kernel for an unused loopback TCP port so a meeting
// browser's CDP endpoint never collides with co-browse's fixed 9222 or with a
// second concurrent meeting. There is a small window between closing the probe
// listener and Chromium binding the port; acceptable because the launcher fails
// loudly (waitForCDPReady times out) rather than silently attaching to a ghost.
func findFreeDebugPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve free CDP port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForCDPReady polls the CDP HTTP endpoint until a page target appears or the
// timeout elapses. Package-level (not a MeetingBrowser method) so it is reusable
// and stays free of manager state.
func waitForCDPReady(debugPort int, timeout time.Duration) error {
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
	return fmt.Errorf("CDP endpoint not ready after %s: %v", timeout, lastErr)
}

// clickJS builds a JS expression that clicks the first element matching selector
// and THROWS when nothing matches. The throw is load-bearing: cdpEvaluate turns a
// JS exception into a Go error, so a stale selector fails loudly during live
// tuning instead of silently reporting a successful click. selector is embedded
// via json.Marshal so any quotes/backslashes are safely escaped.
func clickJS(selector string) string {
	sel, _ := json.Marshal(selector)
	return fmt.Sprintf(
		`(function(){var el=document.querySelector(%s);`+
			`if(!el){throw new Error("selector not found: "+%s);}`+
			`el.scrollIntoView();el.click();return true;})()`,
		sel, sel)
}

// typeJS builds a JS expression that focuses the first element matching selector
// and sets its value using the native value setter (so React/Angular-controlled
// inputs, like Meet's name field, observe the change), then dispatches input and
// change events. Throws when the selector matches nothing (see clickJS). Both
// selector and text are json.Marshal-escaped.
func typeJS(selector, text string) string {
	sel, _ := json.Marshal(selector)
	val, _ := json.Marshal(text)
	return fmt.Sprintf(
		`(function(){var el=document.querySelector(%s);`+
			`if(!el){throw new Error("selector not found: "+%s);}`+
			`el.focus();`+
			`var proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;`+
			`var setter=Object.getOwnPropertyDescriptor(proto,'value').set;`+
			`setter.call(el,%s);`+
			`el.dispatchEvent(new Event('input',{bubbles:true}));`+
			`el.dispatchEvent(new Event('change',{bubbles:true}));return true;})()`,
		sel, sel, val)
}

// cdpEvaluate runs a JS expression in the page and returns its by-value result.
//
// It hardens the raw cdpCommand in two ways that matter for the join flow:
//   - returnByValue:true so the caller reads an actual JSON value (a bool, a
//     number, a string) rather than an opaque RemoteObject handle.
//   - It inspects result.exceptionDetails and returns a Go error on a JS throw.
//     cdpCommand only surfaces PROTOCOL errors (msg["error"]); a JS runtime
//     exception comes back as a normal result, so without this a throwing click
//     on a missing selector would masquerade as success — the worst outcome for
//     the human tuning selectors against a live Meet.
func cdpEvaluate(debugPort int, expression string) (any, error) {
	res, err := cdpCommand(debugPort, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	if exc, ok := res["exceptionDetails"]; ok && exc != nil {
		return nil, fmt.Errorf("javascript exception: %s", describeCDPException(exc))
	}
	result, ok := res["result"].(map[string]any)
	if !ok {
		return nil, nil
	}
	return result["value"], nil
}

// describeCDPException pulls a human-readable message out of a CDP
// exceptionDetails object, preferring the thrown exception's description.
func describeCDPException(exc any) string {
	m, ok := exc.(map[string]any)
	if !ok {
		return fmt.Sprint(exc)
	}
	if e, ok := m["exception"].(map[string]any); ok {
		if d, ok := e["description"].(string); ok && d != "" {
			return d
		}
		if v, ok := e["value"]; ok {
			return fmt.Sprint(v)
		}
	}
	if t, ok := m["text"].(string); ok && t != "" {
		return t
	}
	return fmt.Sprint(m)
}

// MeetingBrowser owns one disposable headed Chromium for a single meeting, its
// dedicated Xvfb display, and its throwaway profile. Safe for concurrent use; the
// reaper goroutines own the single Wait() for each child process, mirroring
// CobrowseManager's process handling.
type MeetingBrowser struct {
	mu         sync.Mutex
	sinkName   string
	debugPort  int
	profileDir string
	display    string
	chromePath string
	cmd        *exec.Cmd
	exited     chan struct{}
	xvfb       *exec.Cmd
	xvfbExited chan struct{}
}

// NewMeetingBrowser creates a meeting browser whose audio will route into the
// given PulseAudio sink (from a NullSinkRecorder). The sink must be loaded before
// Start so the browser's PULSE_SINK target exists at launch.
func NewMeetingBrowser(sinkName string) *MeetingBrowser {
	return &MeetingBrowser{sinkName: sinkName}
}

// DebugPort returns the CDP port the browser listens on (0 before Start).
func (b *MeetingBrowser) DebugPort() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.debugPort
}

// Display returns the X display the browser renders on ("" before Start).
func (b *MeetingBrowser) Display() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.display
}

// Start launches the headed Chromium on a fresh Xvfb display with a throwaway
// profile, routing its audio into the meeting's null sink. It blocks until the
// CDP endpoint is ready so the first Navigate does not race the launch.
func (b *MeetingBrowser) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cmd != nil {
		return fmt.Errorf("meeting browser already started")
	}
	if b.sinkName == "" {
		return fmt.Errorf("meeting browser has no audio sink; construct with NewMeetingBrowser(sinkName)")
	}

	chrome, err := findChromium()
	if err != nil {
		return err
	}

	// Fresh, unique profile per meeting: Chrome locks a --user-data-dir to one
	// process, so a disposable dir both avoids colliding with the co-browse
	// profile and lets parallel meetings run without stomping each other.
	profileDir, err := os.MkdirTemp("", "citadel-meeting-profile-")
	if err != nil {
		return fmt.Errorf("create meeting profile dir: %w", err)
	}

	debugPort, err := findFreeDebugPort()
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return err
	}

	// Meeting nodes are typically headless, so always run on a dedicated Xvfb
	// virtual display (no shared-desktop mode here, unlike co-browse). A managed
	// Xvfb has no GPU, so force software rendering.
	xvfb, display, err := startManagedXvfb(xvfbResolution())
	if err != nil {
		_ = os.RemoveAll(profileDir)
		return err
	}

	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:  debugPort,
		profileDir: profileDir,
		stealth:    stealthEnabled(),
		userAgent:  os.Getenv(EnvCobrowseUserAgent),
		softwareGL: true,
	})

	cmd := exec.Command(chrome, args...)
	// Compose BOTH env transforms: DISPLAY pins the virtual display, PULSE_SINK
	// routes this browser's (and only this browser's) audio into the meeting sink
	// so the recorder captures exactly the call.
	cmd.Env = withPulseSink(withDisplay(os.Environ(), display), b.sinkName)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		if xvfb.Process != nil {
			_ = xvfb.Process.Kill()
		}
		_ = os.RemoveAll(profileDir)
		return fmt.Errorf("launch meeting chromium: %w", err)
	}

	b.cmd = cmd
	b.chromePath = chrome
	b.debugPort = debugPort
	b.profileDir = profileDir
	b.display = display
	b.xvfb = xvfb

	// Reap each child so a crash is observable and no zombie is left. Stop/Close
	// signal the kill and wait on these channels; they never call Wait directly.
	exited := make(chan struct{})
	b.exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	xvfbExited := make(chan struct{})
	b.xvfbExited = xvfbExited
	go func() {
		_ = xvfb.Wait()
		close(xvfbExited)
	}()

	if err := waitForCDPReady(debugPort, 20*time.Second); err != nil {
		// Best-effort teardown so a browser that launched but never exposed CDP
		// does not leak a process, display, or profile dir.
		b.closeLocked()
		return fmt.Errorf("meeting browser launched but CDP not ready: %w", err)
	}
	return nil
}

// Navigate drives the browser to a URL over CDP.
func (b *MeetingBrowser) Navigate(url string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return fmt.Errorf("meeting browser not started")
	}
	_, err := cdpCommand(b.debugPort, "Page.navigate", map[string]any{"url": url})
	return err
}

// Click clicks the first element matching selector, erroring if none matches.
func (b *MeetingBrowser) Click(selector string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return fmt.Errorf("meeting browser not started")
	}
	_, err := cdpEvaluate(b.debugPort, clickJS(selector))
	return err
}

// Type sets the value of the first element matching selector, erroring if none
// matches.
func (b *MeetingBrowser) Type(selector, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return fmt.Errorf("meeting browser not started")
	}
	_, err := cdpEvaluate(b.debugPort, typeJS(selector, text))
	return err
}

// Evaluate runs a JS expression and returns its by-value result. A JS throw is
// returned as a Go error (see cdpEvaluate).
func (b *MeetingBrowser) Evaluate(expression string) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cmd == nil {
		return nil, fmt.Errorf("meeting browser not started")
	}
	return cdpEvaluate(b.debugPort, expression)
}

// Close tears down the browser, its Xvfb, and its throwaway profile. Safe to call
// once; safe when never fully started.
func (b *MeetingBrowser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeLocked()
}

// closeLocked performs teardown; caller holds b.mu. Kills the browser first, then
// the Xvfb (so the browser is never left without a display mid-shutdown), then
// removes the profile dir. Bounded waits avoid a hung child wedging shutdown.
func (b *MeetingBrowser) closeLocked() error {
	var firstErr error
	if b.cmd != nil && b.cmd.Process != nil {
		if err := b.cmd.Process.Kill(); err != nil && !isProcessGoneErr(err) {
			firstErr = fmt.Errorf("kill meeting browser: %w", err)
		}
		if b.exited != nil {
			select {
			case <-b.exited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	b.cmd = nil
	b.exited = nil

	if b.xvfb != nil && b.xvfb.Process != nil {
		if err := b.xvfb.Process.Kill(); err != nil && !isProcessGoneErr(err) && firstErr == nil {
			firstErr = fmt.Errorf("kill meeting Xvfb: %w", err)
		}
		if b.xvfbExited != nil {
			select {
			case <-b.xvfbExited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	b.xvfb = nil
	b.xvfbExited = nil
	b.display = ""

	if b.profileDir != "" {
		if err := os.RemoveAll(b.profileDir); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("remove meeting profile dir: %w", err)
		}
		b.profileDir = ""
	}
	return firstErr
}
