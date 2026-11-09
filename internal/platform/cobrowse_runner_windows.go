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

// defaultHeadlessWindowSize is the fixed default viewport of a headless session.
// It matches the screencast server's default maximum frame size.
const defaultHeadlessWindowSize = "1280,720"

// windowsCDPReadyTimeout bounds how long a launch waits for the automation
// endpoint before classifying the browser as blocked.
const windowsCDPReadyTimeout = 30 * time.Second

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

func (r windowsHeadlessRunner) Launch(profileDir, startURL string) (*cobrowseProc, error) {
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
		ua, err = headedUserAgent(info, filepath.Dir(profileDir))
		if err != nil {
			return nil, err
		}
	}
	return startHeadless(info, profileDir, startURL, ua)
}

// startHeadless launches one headless browser in a kill-on-close job and waits
// for its CDP endpoint.
func startHeadless(info BrowserInfo, profileDir, startURL, userAgent string) (*cobrowseProc, error) {
	debugPort, err := findFreeDebugPort()
	if err != nil {
		return nil, err
	}
	args := buildChromeArgs(cobrowseLaunchOptions{
		debugPort:  debugPort,
		profileDir: profileDir,
		startURL:   startURL,
		stealth:    stealthEnabled(),
		userAgent:  userAgent,
		headless:   true,
		windowSize: defaultHeadlessWindowSize,
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
		debugPort:  debugPort,
		browser:    info.Kind,
		browserPID: pidOf(cmd),
		exited:     exited,
		stop: func() error {
			once.Do(func() { stopErr = stopJob(job, exited) })
			return stopErr
		},
	}

	if err := waitForCDPOrExit(debugPort, windowsCDPReadyTimeout, exited); err != nil {
		_ = proc.stop()
		select {
		case <-exited:
			code := -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
			return nil, fmt.Errorf("%s exited during headless launch (exit code %d); run `citadel local doctor`", info.Kind, code)
		default:
		}
		return nil, fmt.Errorf("%w: %s started but never opened its automation port on 127.0.0.1:%d within %s. "+
			"On a managed (work or school) computer this is usually a policy that disables remote debugging; "+
			"try a per-user Chrome install or a personal computer", ErrBrowserAutomationBlocked, info.Kind, debugPort, windowsCDPReadyTimeout)
	}
	return proc, nil
}

// uaCache maps an installed browser binary (path, size, mtime) to its derived
// non-headless user agent. A browser update replaces the binary, which changes
// the key, so the version in the UA never goes stale.
var (
	uaCacheMu sync.Mutex
	uaCache   = map[string]string{}
)

// headedUserAgent returns the user agent this browser would send outside
// headless mode. The first call for a binary launches it once, headless, in a
// throwaway profile under baseDir, reads /json/version, and stops it.
func headedUserAgent(info BrowserInfo, baseDir string) (string, error) {
	key := info.Path
	if fi, err := os.Stat(info.Path); err == nil {
		key = fmt.Sprintf("%s|%d|%d", info.Path, fi.Size(), fi.ModTime().UnixNano())
	}
	uaCacheMu.Lock()
	defer uaCacheMu.Unlock()
	if ua, ok := uaCache[key]; ok {
		return ua, nil
	}
	probeDir := filepath.Join(baseDir, "ua-probe-"+newSessionID())
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		return "", fmt.Errorf("create user-agent probe dir: %w", err)
	}
	defer removeSessionDir(probeDir)
	proc, err := startHeadless(info, probeDir, "about:blank", "")
	if err != nil {
		return "", err
	}
	raw, verr := browserDefaultUserAgent(proc.debugPort)
	_ = proc.stop()
	if verr != nil {
		return "", fmt.Errorf("read %s user agent: %w", info.Kind, verr)
	}
	ua := deheadlessUserAgent(raw)
	if ua == "" {
		return "", fmt.Errorf("read %s user agent: empty", info.Kind)
	}
	uaCache[key] = ua
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
		} else if owner == self {
			continue
		} else if processAliveWithStart(owner, start) {
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

func removeSessionDir(dir string) { _ = removeAllRetry(dir, 25, 200*time.Millisecond) }

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
