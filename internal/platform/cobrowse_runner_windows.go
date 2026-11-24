//go:build windows

// internal/platform/cobrowse_runner_windows.go
//
// Windows co-browse runner (aceteam#13503 S1, design section 3.3).
//
//   - Discovery: installed Chrome or Edge (cobrowse_discovery.go). No Docker,
//     WSL2 or Xvfb is involved.
//   - Launch: new headless mode with a fixed window size, so the session never
//     takes over the user's screen or focus. The loopback screencast viewer is
//     how a person sees and drives it.
//   - Lifetime: the browser is created suspended, assigned to a Job Object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, then resumed. Every process Chrome
//     spawns inherits the job, and the kernel closes the job handle when the
//     owner exits for any reason (including a crash or a forced kill), which
//     terminates the whole browser tree. stop terminates the job explicitly.
//   - Sweep: never kills by PID. Because the job already killed the browser
//     with its owner, the sweep only removes session dirs whose recorded owner
//     process is gone, identified by PID plus process creation time so a reused
//     PID is not mistaken for the owner.
package platform

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// stillActive is GetExitCodeProcess's STILL_ACTIVE sentinel.
const stillActive = 259

type windowsHeadlessRunner struct{}

func newDefaultBrowserRunner() browserRunner { return windowsHeadlessRunner{} }

func (windowsHeadlessRunner) Name() string { return "headless" }

func (windowsHeadlessRunner) Discover() (BrowserInfo, error) {
	return discoverWindowsBrowser(systemWindowsBrowserProbe())
}

// PrepareBaseDir creates (or verifies) the owner-only session parent and refuses
// to launch beneath one that fails the check.
func (windowsHeadlessRunner) PrepareBaseDir(baseDir string) error {
	if err := ensureCobrowseBaseDir(baseDir); err != nil {
		return fmt.Errorf("co-browse session directory is not private: %w", err)
	}
	return nil
}

func (windowsHeadlessRunner) SweepOrphans(baseDir string) { sweepWindowsSessions(baseDir) }

func (r windowsHeadlessRunner) Launch(profileDir, startURL string, lo LaunchOptions) (*cobrowseProc, error) {
	info, err := r.Discover()
	if err != nil {
		return nil, err
	}
	// Stealth is on by default (founder decision for local mode). New headless
	// mode reports "HeadlessChrome" in its user agent, so derive this
	// browser's real UA from a short probe launch (cached per installed
	// binary) and pass it as --user-agent. An explicit EnvCobrowseUserAgent
	// still wins, and with stealth off the browser's own UA is left alone.
	ua := os.Getenv(EnvCobrowseUserAgent)
	if ua == "" && stealthEnabled() {
		// Probes only ever load about:blank in a throwaway dir. The session
		// itself launches exactly once, so its start URL (which may be a
		// one-time link) is never loaded twice and no discarded visit leaves
		// state in an encrypted profile.
		ua = headedUserAgent(info, filepath.Dir(profileDir))
	}
	return startHeadless(info, profileDir, startURL, ua, lo)
}

// startHeadless launches one headless browser in a kill-on-close job and waits
// for its CDP endpoint. The browser picks its own debug port
// (--remote-debugging-port=0) and reports it in DevToolsActivePort inside the
// owner-only profile dir, so there is no probe-then-bind window in which
// another local process could take the port first. The endpoint is then
// checked against the browser id from that file before it is trusted.
func startHeadless(info BrowserInfo, profileDir, startURL, userAgent string, lo LaunchOptions) (*cobrowseProc, error) {
	_ = os.Remove(filepath.Join(profileDir, devToolsActivePortFile))
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:   0,
		profileDir:  profileDir,
		startURL:    startURL,
		stealth:     stealthEnabled(),
		userAgent:   userAgent,
		headless:    true,
		windowSize:  defaultHeadlessWindowSize,
		proxyServer: lo.ProxyServer,
		quiet:       lo.Quiet,
	})
	cmd := exec.Command(info.Path, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	job, err := startInKillOnCloseJob(cmd)
	if err != nil {
		return nil, fmt.Errorf("launch %s: %w", info.Kind, err)
	}

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	var once sync.Once
	var stopErr error
	proc := &cobrowseProc{
		browser:    info.Kind,
		browserPID: pidOf(cmd),
		exited:     exited,
		stop: func() error {
			once.Do(func() { stopErr = stopJob(job, exited) })
			return stopErr
		},
	}

	fail := func(cause error) (*cobrowseProc, error) {
		// Sample before stopping: stop waits for the process, so checking
		// afterwards always reads "exited" and would blame a browser that was
		// merely slow (and then killed by us) for dying on its own (#1795).
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

// startInKillOnCloseJob starts cmd suspended, assigns it to a new kill-on-close
// Job Object, then resumes it, so no child process can be created before the
// browser is inside the job. The job handle is not inheritable. On any failure
// the process is terminated and reaped and the job closed: a browser never runs
// outside a job.
func startInKillOnCloseJob(cmd *exec.Cmd) (windows.Handle, error) {
	job, err := newKillOnCloseJob()
	if err != nil {
		return 0, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	pid := uint32(cmd.Process.Pid)
	fail := func(cause error) (windows.Handle, error) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = windows.CloseHandle(job)
		return 0, cause
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fail(fmt.Errorf("open browser process: %w", err))
	}
	err = windows.AssignProcessToJobObject(job, ph)
	_ = windows.CloseHandle(ph)
	if err != nil {
		return fail(fmt.Errorf("assign browser to job object: %w", err))
	}
	if err := resumeProcessThreads(pid); err != nil {
		return fail(err)
	}
	return job, nil
}

// newKillOnCloseJob creates an unnamed, non-inheritable Job Object that kills
// every process in it when its last handle closes.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("configure job object: %w", err)
	}
	return job, nil
}

// resumeProcessThreads resumes every thread of a suspended process. A process
// created with CREATE_SUSPENDED has exactly one (its main thread).
func resumeProcessThreads(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot threads: %w", err)
	}
	defer windows.CloseHandle(snap)
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			continue
		}
		if _, rerr := windows.ResumeThread(th); rerr == nil {
			resumed++
		}
		_ = windows.CloseHandle(th)
	}
	if resumed == 0 {
		return fmt.Errorf("resume browser main thread: no thread of pid %d could be resumed", pid)
	}
	return nil
}

// stopJob terminates every process in the job, waits (bounded) for the root
// browser to be reaped, and closes the job handle. Closing the handle alone
// would also kill the tree (kill-on-close); terminating first makes the order
// deterministic before profile removal.
func stopJob(job windows.Handle, exited <-chan struct{}) error {
	var err error
	if termErr := windows.TerminateJobObject(job, 1); termErr != nil {
		err = fmt.Errorf("terminate browser job: %w", termErr)
	}
	waitBounded(exited)
	if cerr := windows.CloseHandle(job); cerr != nil && err == nil {
		err = fmt.Errorf("close browser job: %w", cerr)
	}
	return err
}

// processStartToken returns a process's creation time as a single integer.
func processStartToken(h windows.Handle) (int64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return int64(creation.HighDateTime)<<32 | int64(creation.LowDateTime), nil
}

func currentProcessStartToken() int64 {
	v, err := processStartToken(windows.CurrentProcess())
	if err != nil {
		return 0
	}
	return v
}

// processAliveWithStart reports whether pid is a running process whose creation
// time equals start. It errs toward "alive" when it cannot tell (access denied,
// unknown errors, or no recorded start time), so a sibling's live session dir is
// never removed on doubt.
func processAliveWithStart(pid int, start int64) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER is "no such process".
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != stillActive {
		return false
	}
	if start == 0 {
		return true
	}
	got, err := processStartToken(h)
	if err != nil {
		return true
	}
	return got == start
}

// windowsSweepGrace keeps the sweep away from a session dir that has no pidfile
// yet because a sibling process is still launching into it.
const windowsSweepGrace = 10 * time.Minute

// sweepWindowsSessions removes session dirs whose owner process is gone. It
// never kills a process: the owner's Job Object already terminated that
// session's browser tree when the owner exited.
func sweepWindowsSessions(baseDir string) {
	if err := ensureCobrowseBaseDir(baseDir); err != nil {
		log.Printf("[cobrowse] refusing session sweep of %s: %v", baseDir, err)
		return
	}
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(baseDir, e.Name())
		owner, _, _ := readSessionPidfile(dir)
		start := readSessionOwnerStart(dir)
		if owner == 0 {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) < windowsSweepGrace {
				continue
			}
		} else if owner == self && (start == 0 || start == currentProcessStartToken()) {
			// Ours. A matching PID with a different start time is a dead
			// predecessor whose PID this process reused: swept below.
			continue
		} else if owner != self && processAliveWithStart(owner, start) {
			continue
		}
		if err := removeAllRetry(dir, 10, 200*time.Millisecond); err != nil {
			log.Printf("[cobrowse] sweep: could not remove stale session dir %s: %v", dir, err)
		}
	}
}

// defaultCobrowseSessionsBaseDir is %LOCALAPPDATA%\Citadel\cobrowse-sessions: a
// per-user location that temp-file cleaners leave alone. Its owner-only DACL is
// applied by ensureCobrowseBaseDir.
func defaultCobrowseSessionsBaseDir() string {
	root := os.Getenv("LOCALAPPDATA")
	if root == "" {
		if d, err := os.UserCacheDir(); err == nil {
			root = d
		} else {
			root = os.TempDir()
		}
	}
	return filepath.Join(root, "Citadel", "cobrowse-sessions")
}

func removeSessionDir(dir string) error {
	return removeAllRetry(dir, sessionRemoveAttempts, sessionRemoveDelay)
}

func findChromiumForOS() (string, error) {
	info, err := discoverWindowsBrowser(systemWindowsBrowserProbe())
	if err != nil {
		return "", err
	}
	return info.Path, nil
}

func systemWindowsBrowserProbe() windowsBrowserProbe {
	return windowsBrowserProbe{
		getenv: os.Getenv,
		isFile: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && fi.Mode().IsRegular()
		},
		appPath:  readAppPathRegistry,
		lookPath: exec.LookPath,
	}
}

// readAppPathRegistry reads the default value of the App Paths key for exe.
func readAppPathRegistry(hive, exe string) string {
	root := registry.CURRENT_USER
	if hive == "HKLM" {
		root = registry.LOCAL_MACHINE
	}
	k, err := registry.OpenKey(root, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+exe, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, valType, err := k.GetStringValue("")
	if err != nil {
		return ""
	}
	if valType == registry.EXPAND_SZ {
		if expanded, err := registry.ExpandString(v); err == nil {
			v = expanded
		}
	}
	return v
}

// sessionOwnerAlive reports whether a session's recorded owner still runs,
// guarding against PID reuse with the recorded creation time.
func sessionOwnerAlive(pid int, start int64) bool { return processAliveWithStart(pid, start) }

// killOrphanSessionProcs is a no-op on Windows: the owner's kill-on-close Job
// Object already ended the session's browser tree when the owner exited.
func killOrphanSessionProcs(string, int, int) {}
