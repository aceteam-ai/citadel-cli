//go:build darwin

// internal/platform/cobrowse_runner_darwin.go
//
// macOS co-browse runner (aceteam#13503 S10, citadel-cli#1635). Local mode
// uses it directly; the node-wide manager keeps it as the host fallback behind
// selectingRunner, whose container selection is unchanged.
//
//   - Discovery: Chrome, Edge, Chromium or Chrome Canary app bundles in
//     /Applications and ~/Applications, then PATH (cobrowse_macos_logic.go).
//   - Launch: new headless mode with a fixed window size, so no window opens
//     and no focus is taken. The decisions that must hold on any OS (sign-in
//     and sync off, the egress proxy arguments) come from buildChromeArgs.
//     --use-mock-keychain keeps a hidden browser off the login Keychain prompt.
//   - Lifetime: macOS has no job object and no parent-death signal. The
//     browser leads its own process group (Setpgid); stop kills the whole
//     group. If the owner dies without stopping, the browser keeps running
//     until the next sweep, which kills the group only after verifying the
//     recorded PID still is this session's browser (see killVerifiedBrowserGroup)
//     and removes the dir.
//   - Identity: the owner is its PID plus its start time, read with
//     sysctl(kern.proc.pid) through golang.org/x/sys/unix (microsecond
//     resolution, no locale parsing as `ps -o lstart` would need). A browser's
//     command line is read with `ps -ww -o command= -p <pid>`, because the
//     sysctl procargs layout is undocumented and `ps` is the stable interface.
package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type darwinHeadlessRunner struct{}

func newDefaultBrowserRunner() browserRunner { return darwinHeadlessRunner{} }

func (darwinHeadlessRunner) Name() string { return "headless" }

func (darwinHeadlessRunner) Discover() (BrowserInfo, error) {
	return discoverMacBrowser(systemMacBrowserProbe())
}

// PrepareBaseDir creates (or verifies) the owner-only session parent and
// refuses to launch beneath one that fails the check (a symlink, another
// user's dir, or one group/other can write).
func (darwinHeadlessRunner) PrepareBaseDir(baseDir string) error {
	if err := ensureCobrowseBaseDir(baseDir); err != nil {
		return fmt.Errorf("co-browse session directory is not private: %w", err)
	}
	return nil
}

func (darwinHeadlessRunner) SweepOrphans(baseDir string) {
	sweepMacSessions(baseDir, realMacSweepDeps())
}

func (r darwinHeadlessRunner) Launch(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error) {
	info, err := r.Discover()
	if err != nil {
		return nil, err
	}
	if err := validatePrivateSessionDir(profileDir); err != nil {
		return nil, err
	}
	// Stealth is on by default; new headless mode would report
	// "HeadlessChrome", so derive the real UA once per installed binary, as the
	// Windows runner does (cobrowse_headless_shared.go).
	ua := os.Getenv(EnvCobrowseUserAgent)
	if ua == "" && stealthEnabled() {
		ua = headedUserAgent(info, filepath.Dir(profileDir))
	}
	return startHeadless(info, profileDir, startURL, ua, lo)
}

// startHeadless launches one headless browser in its own process group and
// waits for its CDP endpoint. The browser picks its own debug port and reports
// it in DevToolsActivePort inside the owner-only profile dir; the endpoint is
// checked against the browser id from that file before it is trusted.
func startHeadless(info BrowserInfo, profileDir, startURL, userAgent string, lo LaunchOptions) (*cobrowseProc, error) {
	_ = os.Remove(filepath.Join(profileDir, devToolsActivePortFile))
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:    0,
		profileDir:   profileDir,
		startURL:     startURL,
		stealth:      stealthEnabled(),
		userAgent:    userAgent,
		headless:     true,
		windowSize:   defaultHeadlessWindowSize,
		proxyServer:  lo.ProxyServer,
		quiet:        lo.Quiet,
		mockKeychain: true,
	})
	cmd := exec.Command(info.Path, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launch %s: %w", info.Kind, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	pgid := cmd.Process.Pid // Setpgid: the browser leads its own group
	var once sync.Once
	var stopErr error
	proc := &cobrowseProc{
		browser:    info.Kind,
		browserPID: pgid,
		exited:     exited,
		stop: func() error {
			once.Do(func() {
				// Whole group, so renderer, GPU and helper processes go with
				// the main process; then wait (bounded) for it to be reaped.
				stopErr = killProcessGroup(pgid)
				waitBounded(exited)
			})
			return stopErr
		},
	}

	fail := func(cause error) (*cobrowseProc, error) {
		// Sample before stopping: stop waits for the process, so checking
		// afterwards always reads "exited" (see the Windows runner, #1795).
		select {
		case <-exited:
			_ = proc.stop()
			code := -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
			return nil, headlessLaunchError(info.Kind, true, code, cause)
		default:
		}
		_ = proc.stop()
		return nil, headlessLaunchError(info.Kind, false, 0, cause)
	}

	port, browserID, err := waitForDevToolsActivePort(profileDir, headlessCDPReadyTimeout, exited)
	if err != nil {
		return fail(err)
	}
	proc.debugPort = port
	proc.browserID = browserID
	if err := verifyCDPBrowserID(port, browserID); err != nil {
		return fail(err)
	}
	if err := waitForCDPOrExit(port, headlessCDPReadyTimeout, exited); err != nil {
		return fail(err)
	}
	return proc, nil
}

// killProcessGroup sends SIGKILL to a whole process group. A group that is
// already gone is success.
func killProcessGroup(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal process group %d", pgid)
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("kill browser process group: %w", err)
	}
	return nil
}

// processStartToken returns a process's start time in microseconds since the
// epoch, from sysctl kern.proc.pid.
func processStartToken(pid int) (int64, error) {
	ki, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	tv := ki.Proc.P_starttime
	v := int64(tv.Sec)*1_000_000 + int64(tv.Usec)
	if v == 0 {
		return 0, fmt.Errorf("pid %d reports no start time", pid)
	}
	return v, nil
}

func currentProcessStartToken() int64 {
	v, err := processStartToken(os.Getpid())
	if err != nil {
		return 0
	}
	return v
}

// sessionOwnerAlive reports whether a session's recorded owner still runs,
// guarding against PID reuse with the recorded start time.
func sessionOwnerAlive(pid int, start int64) bool {
	return macOwnerAliveWithStart(pid, start, func(p int) error { return syscall.Kill(p, 0) }, processStartToken)
}

// processCommandLine reads a process's full command line through ps.
func processCommandLine(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", fmt.Errorf("no command line for pid %d", pid)
	}
	return s, nil
}

func realMacSweepDeps() macSweepDeps {
	return macSweepDeps{
		self:       os.Getpid(),
		selfStart:  currentProcessStartToken(),
		ownerAlive: sessionOwnerAlive,
		cmdline:    processCommandLine,
		pgid:       syscall.Getpgid,
		killGroup:  killProcessGroup,
		removeDir:  removeSessionDir,
	}
}

// killOrphanSessionProcs kills an interrupted persistent-profile session's
// browser group, only while the recorded PID still verifies as that session's
// browser. There is no Xvfb on macOS.
func killOrphanSessionProcs(dir string, browserPID, _ int) {
	killVerifiedBrowserGroup(dir, browserPID, realMacSweepDeps())
}

func systemMacBrowserProbe() macBrowserProbe {
	home, _ := os.UserHomeDir()
	return macBrowserProbe{
		getenv: os.Getenv,
		home:   home,
		isExec: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
		},
		lookPath: exec.LookPath,
	}
}

func findChromiumForOS() (string, error) {
	info, err := discoverMacBrowser(systemMacBrowserProbe())
	if err != nil {
		return "", err
	}
	return info.Path, nil
}

func defaultCobrowseSessionsBaseDir() string {
	cache, err := os.UserCacheDir()
	return macDefaultBaseDir(cache, err, os.TempDir())
}

func removeSessionDir(dir string) error {
	return removeSessionDirRetry(dir, sessionRemoveAttempts, sessionRemoveDelay)
}
