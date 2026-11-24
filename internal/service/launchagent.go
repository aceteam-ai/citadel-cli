// internal/service/launchagent.go
//
// The per-user LaunchAgent that keeps `citadel work` running for a standard
// macOS user (citadel-cli#1903 M3, the macOS counterpart of #1867 C3/C5).
// launchd is the supervisor here: RunAtLoad starts it at sign-in and KeepAlive
// restarts a crash, a self-heal exit or an update restart, so none of the
// Windows `--background` supervisor is ported.
//
// The code is platform-independent on purpose. Every side effect goes through
// a LaunchAgent field (the launchctl runner, the executable's owner check, the
// home directory), so the install, start, stop and state logic is tested on the
// Linux CI host with a fake runner and a temp home. The production wiring is
// NewUserLaunchAgent in user_darwin.go.
//
// It reuses the label and plist rendering of the `citadel init` agent
// (launchd_plist.go), so an agent `init` installed earlier is adopted: the same
// path, label and keys, rewritten in place only when its content differs.
package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LaunchAgentLogMaxBytes bounds each launchd log file. launchd appends to
// StandardOutPath and StandardErrorPath forever, so Start truncates a file past
// this and the doctor warns about one.
const LaunchAgentLogMaxBytes = 50 << 20

// launchctlServiceNotFound is launchctl's "Could not find service" exit status.
// It is the only exit that means the job is absent from the domain; any other
// failure (no such domain over SSH, a permission error) says nothing about
// whether the worker runs, so it is an error, never "stopped".
const launchctlServiceNotFound = 113

// errNeedDesktopSession is the fail-closed answer where launchd's per-user GUI
// domain cannot be reached. The legacy load/unload verbs act on the caller's
// own session domain (an SSH login has a different one), so they are never
// used: they could start or stop a job that gui/<uid> does not show.
const errNeedDesktopSession = "run this from your logged-in desktop session (Terminal on the Mac itself, or the setup page), not over SSH"

// StartWait bounds how long Start waits for the worker to show a running PID
// after bootstrap; StartPoll is the gap between checks. Both are vars so tests
// need not sleep. The caller's context bounds them further.
var (
	StartWait = 10 * time.Second
	StartPoll = 250 * time.Millisecond
)

// launchctlOutputMax caps what a launchctl call's output is read as.
const launchctlOutputMax = 64 << 10

// LaunchctlExit is returned by a runner when launchctl ran and exited
// non-zero. Any other runner error (launchctl missing, a timeout) is a hard
// failure and is never read as "the job is not loaded".
type LaunchctlExit struct {
	Code   int
	Output string
}

func (e *LaunchctlExit) Error() string {
	return fmt.Sprintf("launchctl exited with status %d: %s", e.Code, strings.TrimSpace(e.Output))
}

// ExeInfo is what the owner check needs to know about an executable.
type ExeInfo struct {
	UID  int
	Mode fs.FileMode
}

// LaunchAgent manages one user's citadel LaunchAgent.
type LaunchAgent struct {
	// Home is the user's home directory; the plist lives under
	// Home/Library/LaunchAgents and the logs under Home/Library/Logs/citadel.
	Home string
	// UID selects the gui/<uid> domain.
	UID int
	// Euid is the process's effective uid; 0 refuses everything, so a LaunchDaemon
	// or a gui/0 agent is never written.
	Euid int
	// Run runs `launchctl args...`, with its output capped, and gives up when
	// ctx does.
	Run func(ctx context.Context, args ...string) ([]byte, error)
	// StatExe reads an executable's owner and mode, following symlinks.
	StatExe func(path string) (ExeInfo, error)
}

// LaunchAgentState is a read-only view for the doctor and the setup page.
type LaunchAgentState struct {
	PlistPath    string
	PlistPresent bool
	// Loaded: launchd knows the job. PID is its process when it is running.
	Loaded bool
	PID    int
	// Disabled: the user switched it off in System Settings > General > Login
	// Items & Extensions (launchctl print-disabled), so launchd will not load it.
	Disabled bool
	// Exe and Args are the plist's ProgramArguments.
	Exe  string
	Args []string
	// PointsAtThis: Exe is the executable asked about and Args are the worker's.
	PointsAtThis bool
	// LogBytes is the combined size of the two launchd log files.
	LogBytes int64
	// Err is a read that failed; the other fields are then partial.
	Err error
}

// agentArgs is what the agent runs: the same `work` the init agent runs.
var agentArgs = []string{"work"}

func (a *LaunchAgent) plistPath() string {
	return filepath.Join(a.Home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (a *LaunchAgent) logDir() string { return filepath.Join(a.Home, "Library", "Logs", "citadel") }

func (a *LaunchAgent) logFiles() []string {
	return []string{filepath.Join(a.logDir(), "citadel.log"), filepath.Join(a.logDir(), "citadel-error.log")}
}

func (a *LaunchAgent) domain() string { return launchdDomainTarget(true, a.UID) }

func (a *LaunchAgent) target() string { return a.domain() + "/" + launchdLabel }

// PlistPath is the agent's plist path.
func (a *LaunchAgent) PlistPath() string { return a.plistPath() }

func (a *LaunchAgent) refuseRoot() error {
	if a.Euid == 0 {
		return errors.New("the macOS worker agent is a per-user LaunchAgent; run this as your own user, not as root or with sudo")
	}
	return nil
}

// checkExe refuses an executable this user does not own, or one another user
// could replace: the LaunchAgent would run it at every sign-in.
func (a *LaunchAgent) checkExe(exe string) error {
	if !filepath.IsAbs(exe) || strings.ContainsAny(exe, "\r\n\x00") {
		return fmt.Errorf("worker agent: executable path %q is not an absolute path", exe)
	}
	info, err := a.StatExe(exe)
	if err != nil {
		return fmt.Errorf("worker agent: could not check %s: %w", exe, err)
	}
	if !info.Mode.IsRegular() {
		return fmt.Errorf("worker agent: %s is not a regular file", exe)
	}
	if info.UID != a.Euid {
		return fmt.Errorf("worker agent: %s is owned by another user, so it will not be started at sign-in. Reinstall citadel as yourself", exe)
	}
	if info.Mode.Perm()&0o022 != 0 {
		return fmt.Errorf("worker agent: %s can be changed by other users (mode %o), so it will not be started at sign-in. Run: chmod go-w %q", exe, info.Mode.Perm(), exe)
	}
	// The directory holding the binary, and the one holding a symlink's target
	// (a Homebrew prefix bin links into the Cellar), must not let group or
	// other swap the file either. Owned by this user or root; Homebrew's
	// Apple Silicon prefix is user-owned and 755, so it passes.
	dirs := []string{filepath.Dir(exe)}
	if real, err := filepath.EvalSymlinks(exe); err == nil && filepath.Dir(real) != dirs[0] {
		dirs = append(dirs, filepath.Dir(real))
	}
	for _, dir := range dirs {
		di, err := a.StatExe(dir)
		if err != nil {
			return fmt.Errorf("worker agent: could not check %s: %w", dir, err)
		}
		if di.UID != a.Euid && di.UID != 0 {
			return fmt.Errorf("worker agent: the folder %s is owned by another user, so %s will not be started at sign-in", dir, exe)
		}
		if di.Mode.Perm()&0o022 != 0 {
			return fmt.Errorf("worker agent: the folder %s can be changed by other users (mode %o), so %s will not be started at sign-in. Run: chmod go-w %q", dir, di.Mode.Perm(), exe, dir)
		}
	}
	return nil
}

func (a *LaunchAgent) render(exe string) string {
	return renderLaunchdPlist(launchdPlistInput{
		Label:     launchdLabel,
		ExecPath:  exe,
		Args:      agentArgs,
		HomeDir:   a.Home,
		LogDir:    a.logDir(),
		RunAtLoad: true,
		KeepAlive: true,
		PathEnv:   launchdServicePATH,
	})
}

// Enable writes the plist for exe (the "start at sign-in" step). It does not
// start the worker. An existing plist with the same content is left alone; a
// different one (an init agent for another path, a moved binary) is replaced
// atomically, and a job already loaded from the old content is booted out so
// Start loads the new one. Every field the plist carries (executable, arguments,
// PATH, HOME, log paths) changes what the worker runs or sees, and launchd reads
// the plist only at load, so a differing plist always needs that restart.
func (a *LaunchAgent) Enable(exe string) error {
	if err := a.refuseRoot(); err != nil {
		return err
	}
	if err := a.checkExe(exe); err != nil {
		return err
	}
	content := a.render(exe)
	pp := a.plistPath()
	if existing, err := os.ReadFile(pp); err == nil && string(existing) == content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(pp), err)
	}
	if err := os.MkdirAll(a.logDir(), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", a.logDir(), err)
	}
	if err := writeFileAtomic(pp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", pp, err)
	}
	return a.stop(context.Background())
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".citadel-agent-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// print reads the job from launchd: loaded, and its PID when running.
func (a *LaunchAgent) print(ctx context.Context) (loaded bool, pid int, err error) {
	out, err := a.Run(ctx, "print", a.target())
	var exit *LaunchctlExit
	switch {
	case err == nil:
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "pid = "); ok {
				if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil && n > 0 {
					pid = n
				}
			}
		}
		return true, pid, nil
	case errors.As(err, &exit) && exit.Code == launchctlServiceNotFound:
		return false, 0, nil
	default:
		return false, 0, fmt.Errorf("launchctl print: %w", err)
	}
}

// disabled reads launchctl print-disabled for this label.
func (a *LaunchAgent) disabled(ctx context.Context) (bool, error) {
	out, err := a.Run(ctx, "print-disabled", a.domain())
	if err != nil {
		return false, fmt.Errorf("launchctl print-disabled: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, `"`+launchdLabel+`"`) {
			continue
		}
		_, v, ok := strings.Cut(line, "=>")
		v = strings.Trim(strings.TrimSpace(v), ";,")
		return ok && (v == "disabled" || v == "true"), nil
	}
	return false, nil
}

// Start loads the agent, which RunAtLoad starts at once, and waits (bounded by
// StartWait and ctx) for launchd to show a running PID: a job that loads but
// exits at once is a failed start, reported with the log path. It is
// idempotent: a job that is already running is left alone, so a second click
// never drops a healthy worker's jobs. It refuses a missing plist and an agent
// the user switched off in Login Items, even if it is running now, since it
// would not start at the next sign-in. Every launchctl call honours ctx.
func (a *LaunchAgent) Start(ctx context.Context) error {
	if err := a.refuseRoot(); err != nil {
		return err
	}
	pp := a.plistPath()
	if _, err := os.Stat(pp); err != nil {
		return fmt.Errorf("the worker agent is not installed (%s): %w", pp, err)
	}
	if off, err := a.disabled(ctx); err != nil {
		return err
	} else if off {
		return errors.New("the Citadel worker is switched off in System Settings > General > Login Items & Extensions; turn it on there, then try again")
	}
	loaded, pid, err := a.print(ctx)
	if err != nil {
		return err
	}
	if loaded && pid > 0 {
		return nil
	}
	a.trimLogs()
	if loaded {
		// Loaded but not running: reload so launchd starts a fresh one.
		if err := a.bootout(ctx); err != nil {
			return err
		}
	}
	if _, err := a.Run(ctx, "bootstrap", a.domain(), pp); err != nil {
		return fmt.Errorf("launchctl bootstrap into %s failed (%v); %s", a.domain(), err, errNeedDesktopSession)
	}
	deadline := time.NewTimer(StartWait)
	defer deadline.Stop()
	for {
		loaded, pid, err := a.print(ctx)
		if err != nil {
			return err
		}
		if loaded && pid > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return a.notRunning(ctx.Err())
		case <-deadline.C:
			return a.notRunning(nil)
		case <-time.After(StartPoll):
		}
	}
}

func (a *LaunchAgent) notRunning(cause error) error {
	msg := fmt.Sprintf("launchd loaded the Citadel worker but it is not running; see %s", filepath.Join(a.logDir(), "citadel-error.log"))
	if cause != nil {
		return fmt.Errorf("%s (%w)", msg, cause)
	}
	return errors.New(msg)
}

func (a *LaunchAgent) bootout(ctx context.Context) error {
	if _, err := a.Run(ctx, "bootout", a.target()); err != nil {
		return fmt.Errorf("launchctl bootout %s failed (%v); %s", a.target(), err, errNeedDesktopSession)
	}
	return nil
}

// Stop makes sure launchd no longer runs the worker. A job that is not loaded
// is success, but only on launchctl's "service not found" exit: any other print
// failure is an error. A bootout that fails is an error only if the job is
// still loaded afterwards, so the caller can trust "nil" to mean the KeepAlive
// job is gone and will not restart. There is no legacy unload fallback: it acts
// on the caller's session domain, not gui/<uid>.
func (a *LaunchAgent) Stop() error { return a.stop(context.Background()) }

func (a *LaunchAgent) stop(ctx context.Context) error {
	loaded, _, err := a.print(ctx)
	if err != nil {
		return err
	}
	if !loaded {
		return nil
	}
	berr := a.bootout(ctx)
	loaded, _, err = a.print(ctx)
	if err != nil {
		return err
	}
	if loaded {
		if berr == nil {
			berr = errors.New("the job is still loaded")
		}
		return fmt.Errorf("could not stop the Citadel worker: %w", berr)
	}
	return nil
}

// Remove deletes the plist (the "no sign-in autostart" step). An absent plist
// is success. It does not stop a running job; Stop does.
func (a *LaunchAgent) Remove() error {
	if err := os.Remove(a.plistPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", a.plistPath(), err)
	}
	return nil
}

// trimLogs truncates a launchd log past LaunchAgentLogMaxBytes (best effort).
func (a *LaunchAgent) trimLogs() {
	for _, f := range a.logFiles() {
		truncateIfOversize(f, LaunchAgentLogMaxBytes)
	}
}

// State reads the agent without changing anything. thisExe is the executable
// the plist is expected to run.
func (a *LaunchAgent) State(thisExe string) LaunchAgentState {
	st := LaunchAgentState{PlistPath: a.plistPath()}
	if data, err := os.ReadFile(st.PlistPath); err == nil {
		st.PlistPresent = true
		args := parseLaunchdProgramArguments(string(data))
		if len(args) > 0 {
			st.Exe, st.Args = args[0], args[1:]
			st.PointsAtThis = st.Exe == thisExe && strings.Join(st.Args, " ") == strings.Join(agentArgs, " ")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		st.Err = err
	}
	loaded, pid, err := a.print(context.Background())
	if err != nil && st.Err == nil {
		st.Err = err
	}
	st.Loaded, st.PID = loaded, pid
	if off, err := a.disabled(context.Background()); err != nil {
		if st.Err == nil {
			st.Err = err
		}
	} else {
		st.Disabled = off
	}
	for _, f := range a.logFiles() {
		if fi, err := os.Lstat(f); err == nil && fi.Mode().IsRegular() {
			st.LogBytes += fi.Size()
		}
	}
	return st
}

// AutostartStatusFromAgent maps an agent state onto UserAutostartStatus (the
// plist is the autostart entry; Login Items is its "switched off by the user").
func AutostartStatusFromAgent(st LaunchAgentState) UserAutostartStatus {
	return UserAutostartStatus{Set: st.PlistPresent, Value: st.PlistPath, Exe: st.Exe, PointsAtThis: st.PointsAtThis, DisabledByUser: st.Disabled}
}
