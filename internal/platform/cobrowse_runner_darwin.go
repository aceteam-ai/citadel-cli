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
//     browser joins a process group led by a citadel-owned placeholder that
//     is reaped last (groupGuard), so a signal never reaches a reused group id;
//     stop and the exit waiter end the whole group. If the owner dies without
//     stopping, the browser keeps running until the next sweep, which kills the
//     group only after verifying the recorded PID still is this session's
//     browser (see killVerifiedBrowserGroup) and removes the dir; a browser it
//     cannot verify or kill keeps its dir for a later retry.
//   - Identity: the owner is its PID plus its start time, read with
//     sysctl(kern.proc.pid) through golang.org/x/sys/unix (microsecond
//     resolution, no locale parsing as `ps -o lstart` would need). A browser's
//     command line is read with `ps -ww -o command= -p <pid>`, because the
//     sysctl procargs layout is undocumented and `ps` is the stable interface.
package platform

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	if baseDir == "" {
		return errors.New("co-browse session directory is unavailable: cannot determine your home folder (set HOME)")
	}
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

	// A citadel-owned placeholder leads the session's process group and is
	// reaped only by groupGuard.teardown, last. While it is unreaped its pid, and
	// so the group id, cannot be given to another process, so every signal sent
	// to the group is sent to our own group. It polls the owner and exits with
	// it, which keeps a crashed owner from leaking it; the browser then still
	// holds the group.
	leader := exec.Command("/bin/sh", "-c", `while kill -0 "$1" 2>/dev/null; do /bin/sleep 1; done`, "sh", strconv.Itoa(os.Getpid()))
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		return nil, fmt.Errorf("start browser process group: %w", err)
	}
	pgid := leader.Process.Pid

	cmd := exec.Command(info.Path, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	exited := make(chan struct{})
	guard := &groupGuard{
		pgid:          pgid,
		signal:        signalProcessGroup,
		browserExited: exited,
		termGrace:     2 * time.Second,
		reapLeader: func() {
			_ = leader.Process.Kill()
			done := make(chan struct{})
			go func() { _ = leader.Wait(); close(done) }()
			waitBounded(done)
		},
	}
	if err := cmd.Start(); err != nil {
		close(exited) // no browser started: do not wait out the TERM grace
		_ = guard.teardown()
		return nil, fmt.Errorf("launch %s: %w", info.Kind, err)
	}
	go func() {
		_ = cmd.Wait()
		close(exited)
		// The leader still holds the group id, so this reaps the browser's
		// helpers without ever signalling a reused id.
		_ = guard.teardown()
	}()

	proc := &cobrowseProc{
		browser:    info.Kind,
		browserPID: cmd.Process.Pid,
		// xvfbPID records the group leader in the pidfile (there is no Xvfb
		// on macOS); the sweep requires the browser to be in that group.
		xvfbPID: pgid,
		exited:  exited,
		stop: func() error {
			err := guard.teardown()
			waitBounded(exited)
			return err
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

// signalProcessGroup sends sig to a whole process group. A group that is
// already gone is success. Callers hold a groupGuard, which keeps the group id
// reserved.
func signalProcessGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal process group %d", pgid)
	}
	if err := syscall.Kill(-pgid, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

// killProcessGroup sends SIGKILL to a group the caller has verified (the
// orphan sweep).
func killProcessGroup(pgid int) error { return signalProcessGroup(pgid, syscall.SIGKILL) }

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
	s := strings.TrimSpace(string(out))
	if s != "" {
		return s, nil
	}
	// ps exits 1 with no output for a PID that does not exist.
	var ee *exec.ExitError
	if err == nil || (errors.As(err, &ee) && ee.ExitCode() == 1) {
		return "", errMacProcGone
	}
	return "", err
}

func realMacSweepDeps() macSweepDeps {
	return macSweepDeps{
		self:       os.Getpid(),
		selfStart:  currentProcessStartToken(),
		ownPgrp:    syscall.Getpgrp(),
		ownerAlive: sessionOwnerAlive,
		cmdline:    processCommandLine,
		pgid:       syscall.Getpgid,
		killGroup:  killProcessGroup,
		removeDir:  removeSessionDir,
	}
}

// killOrphanSessionProcs kills an interrupted persistent-profile session's
// browser group, only while the recorded PID still verifies as that session's
// browser; leaderPID is the recorded group leader. A browser that could not be
// verified and killed is an error, so the caller never reports it cleaned.
func killOrphanSessionProcs(dir string, browserPID, leaderPID int) error {
	return killVerifiedBrowserGroup(dir, browserPID, leaderPID, realMacSweepDeps())
}

// The shared profile-user reaper (reapProfileUsers) reads /proc, which macOS
// lacks; here it reads `ps` instead, so a helper still using a session's
// --user-data-dir fails persistence and cleanup rather than being missed.
func init() {
	listReapProcs = listMacReapProcs
	lookupReapProc = lookupMacReapProc
}

func macReapProc(r macPSRow) (reapProc, bool) {
	start, err := processStartToken(r.pid)
	if err != nil {
		return reapProc{}, false
	}
	return reapProc{pid: r.pid, start: uint64(start), uid: r.uid, args: strings.Fields(r.cmd), cmd: r.cmd}, true
}

func listMacReapProcs() ([]reapProc, error) {
	return listMacReapProcsFrom(func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-axo", "pid=,uid=,command=").Output()
		return string(out), err
	}, processStartToken)
}

func lookupMacReapProc(pid int) (reapProc, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-o", "pid=,uid=,command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return reapProc{}, false
	}
	for _, r := range parseMacPS(string(out)) {
		if r.pid == pid {
			return macReapProc(r)
		}
	}
	return reapProc{}, false
}

func systemMacBrowserProbe() macBrowserProbe {
	home, _ := UserHomeDir()
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
	home, err := UserHomeDir()
	dir, err := macDefaultBaseDir(home, err)
	if err != nil {
		log.Printf("[cobrowse] %v", err)
		return "" // PrepareBaseDir refuses an empty base dir
	}
	return dir
}

func removeSessionDir(dir string) error {
	return removeSessionDirRetry(dir, sessionRemoveAttempts, sessionRemoveDelay)
}
