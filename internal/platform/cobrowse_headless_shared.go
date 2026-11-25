//go:build windows || darwin

// internal/platform/cobrowse_headless_shared.go
//
// Pieces shared by the headless co-browse runners (windowsHeadlessRunner and
// darwinHeadlessRunner): the default viewport, the derived non-headless user
// agent, the CDP wait and the launch-failure wording. Only how the browser is
// started and tied to its owner differs per OS.
package platform

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// defaultHeadlessWindowSize is the fixed default viewport of a headless session.
// It matches the screencast server's default maximum frame size.
const defaultHeadlessWindowSize = "1280,720"

// headlessCDPReadyTimeout bounds how long a launch waits for the automation
// endpoint before classifying the browser as blocked.
const headlessCDPReadyTimeout = 30 * time.Second

func uaCacheKey(info BrowserInfo) string {
	key := info.Path
	if fi, err := os.Stat(info.Path); err == nil {
		key = fmt.Sprintf("%s|%d|%d", info.Path, fi.Size(), fi.ModTime().UnixNano())
	}
	return key
}

// uaEntry is one binary's derived user agent, computed at most once at a time.
type uaEntry struct {
	done chan struct{}
	ua   string
	err  error
	at   time.Time
}

// uaCache maps an installed browser binary (path, size, mtime) to its derived
// non-headless user agent. A browser update replaces the binary, which changes
// the key, so the version in the UA never goes stale. A failed probe is cached
// for uaProbeRetryAfter so a machine that cannot run the probe does not pay
// for it on every session start.
var (
	uaCacheMu         sync.Mutex
	uaCache           = map[string]*uaEntry{}
	uaProbeRetryAfter = 10 * time.Minute
	uaProbeLaunch     = func(info BrowserInfo, dir string) (string, error) {
		proc, err := startHeadless(info, dir, "about:blank", "", LaunchOptions{Quiet: true})
		if err != nil {
			return "", err
		}
		defer proc.stop()
		return browserDefaultUserAgent(proc.debugPort)
	}
)

// headedUserAgent returns the user agent this browser would send outside
// headless mode, or "" (the browser's own) when it cannot be derived: a
// failed probe never fails the session. The first call for a binary launches
// it once, headless, in a throwaway profile under baseDir; concurrent callers
// wait for that one probe without holding the cache lock.
func headedUserAgent(info BrowserInfo, baseDir string) string {
	key := uaCacheKey(info)
	uaCacheMu.Lock()
	e, ok := uaCache[key]
	if ok {
		// Read e.err only after e.done is closed (the prober writes it before
		// closing), never while a probe is still running.
		select {
		case <-e.done:
			if e.err != nil && time.Since(e.at) >= uaProbeRetryAfter {
				ok = false
			}
		default:
		}
	}
	if !ok {
		e = &uaEntry{done: make(chan struct{})}
		uaCache[key] = e
		uaCacheMu.Unlock()
		e.ua, e.err = probeUserAgent(info, baseDir)
		if e.err != nil {
			// One immediate retry: a cold first launch on a slow machine can
			// time out where a second, warm one succeeds. A machine where the
			// browser cannot run at all pays this only once per retry window.
			e.ua, e.err = probeUserAgent(info, baseDir)
		}
		e.at = time.Now()
		if e.err != nil {
			log.Printf("[cobrowse] could not derive %s's user agent (%v); the session uses the browser's own", info.Kind, e.err)
		}
		close(e.done)
	} else {
		uaCacheMu.Unlock()
		<-e.done
	}
	if e.err != nil {
		return ""
	}
	return e.ua
}

func probeUserAgent(info BrowserInfo, baseDir string) (string, error) {
	probeDir := filepath.Join(baseDir, "ua-probe-"+newSessionID())
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		return "", fmt.Errorf("create user-agent probe dir: %w", err)
	}
	defer func() { _ = removeSessionDir(probeDir) }()
	raw, err := uaProbeLaunch(info, probeDir)
	if err != nil {
		return "", err
	}
	ua := deheadlessUserAgent(raw)
	if ua == "" {
		return "", errors.New("empty user agent")
	}
	return ua, nil
}

// waitForCDPOrExit polls the CDP endpoint until it answers, the browser exits,
// or timeout elapses.
func waitForCDPOrExit(debugPort int, timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := pickTarget(debugPort); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-exited:
			return fmt.Errorf("browser exited before CDP was ready: %v", lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("CDP endpoint not ready after %s: %v", timeout, lastErr)
}

// headlessLaunchError words a failed headless launch. exitedOnItsOwn is true
// only when the browser process was already gone before we stopped it.
func headlessLaunchError(kind string, exitedOnItsOwn bool, code int, cause error) error {
	if exitedOnItsOwn {
		return fmt.Errorf("%s exited during headless launch (exit code %d); run `citadel local doctor`", kind, code)
	}
	if errors.Is(cause, errCDPImpostor) {
		return fmt.Errorf("%s debug port answered as a different browser; refusing the session: %w", kind, cause)
	}
	return fmt.Errorf("%w: %s started but never opened its automation port within %s. "+
		"On a managed (work or school) computer this is usually a policy that disables remote debugging; "+
		"try a per-user Chrome install or a personal computer (last error: %v)", ErrBrowserAutomationBlocked, kind, headlessCDPReadyTimeout, cause)
}
