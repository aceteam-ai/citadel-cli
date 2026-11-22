// cmd/work_background.go
//
// Per-user worker for Windows (citadel-cli#1867 C3). A standard user cannot
// install the LocalSystem service, so `citadel work --background` keeps the
// worker running instead:
//
//   - the launcher (`work --background`, what the HKCU Run value starts at
//     logon) spawns `work --supervise` hidden and detached, then returns;
//   - the supervisor holds a per-user lock, starts the real worker as its child
//     with --supervisor-pid and --supervisor-lock, and restarts it
//     (service.RunSupervisor). After an update restart it hands over to a fresh
//     supervisor started from the updated binary;
//   - the worker treats itself as service-managed only while that supervisor is
//     its live parent and still holds the lock (supervisedByUserSupervisor), so
//     self-heal, remote restart and update-restart exits are brought back.
//
// `citadel worker stop` writes a stop request naming the supervisor's PID.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/service"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

var (
	// workBackground is the public launcher flag.
	workBackground bool
	// workSupervise runs the supervisor loop (hidden; set by the launcher).
	workSupervise bool
	// workSupervisorPID names the supervisor that started this worker (hidden;
	// set by the supervisor). It is only trusted after supervisedByUserSupervisor
	// checks it against the live parent and the supervisor lock.
	workSupervisorPID int
	// workSupervisorLock is that supervisor's lock file, passed in by the
	// supervisor so the worker never re-resolves the node dir to find it.
	workSupervisorLock string
)

const (
	backgroundFlag     = "background"
	superviseFlag      = "supervise"
	supervisorPIDFlag  = "supervisor-pid"
	supervisorLockFlag = "supervisor-lock"

	backgroundLogName     = "worker-background.log"
	backgroundLogMaxBytes = 10 << 20
	// supervisorStopPoll is how often the supervisor checks for a stop request.
	supervisorStopPoll = time.Second
)

// supervisorLockRetry is how long a starting supervisor waits for the lock: a
// successor started by a handover waits here while its predecessor releases.
// A duplicate launch simply gives up after it.
var supervisorLockRetry = 10 * time.Second

func init() {
	workCmd.Flags().BoolVar(&workBackground, backgroundFlag, false, "Windows: start the worker hidden in the background under a per-user supervisor that restarts it, then return (stop it with 'citadel worker stop')")
	workCmd.Flags().BoolVar(&workSupervise, superviseFlag, false, "internal: run the per-user worker supervisor")
	workCmd.Flags().IntVar(&workSupervisorPID, supervisorPIDFlag, 0, "internal: PID of the per-user supervisor that started this worker")
	workCmd.Flags().StringVar(&workSupervisorLock, supervisorLockFlag, "", "internal: lock file of the per-user supervisor that started this worker")
	_ = workCmd.Flags().MarkHidden(superviseFlag)
	_ = workCmd.Flags().MarkHidden(supervisorPIDFlag)
	_ = workCmd.Flags().MarkHidden(supervisorLockFlag)
}

// handleBackgroundModes runs the launcher or the supervisor when asked and
// reports whether it did (the process then exits instead of running a worker).
// It must run before runWork touches any node state or takes the worker lock.
func handleBackgroundModes() bool {
	if !workBackground && !workSupervise && workSupervisorPID == 0 {
		return false
	}
	if runtime.GOOS != "windows" {
		fmt.Fprintln(os.Stderr, "Error: --background is the per-user worker for Windows. On Linux and macOS run 'citadel service install' to keep the worker running.")
		os.Exit(2)
	}
	switch {
	case workSupervise:
		os.Exit(runWorkSupervisor())
	case workBackground:
		os.Exit(runWorkLauncher())
	}
	// --supervisor-pid: this is the supervised worker; continue into runWork.
	// The flag alone decides the update restart: a failed lock probe must never
	// turn it into a spawned copy that the supervisor's job then kills.
	update.SetSupervisedRestart(true)
	return false
}

// stripWorkModeFlags removes the background-mode flags (in either the
// `--flag value` or `--flag=value` form) so a forwarded command line can never
// re-enter a mode it already left.
func stripWorkModeFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, hasValue := strings.CutPrefix(a, "--")
		if !hasValue {
			out = append(out, a)
			continue
		}
		base, _, inline := strings.Cut(name, "=")
		switch base {
		case backgroundFlag, superviseFlag:
			continue
		case supervisorPIDFlag, supervisorLockFlag:
			if !inline && i+1 < len(args) {
				i++ // drop the separate value too
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

// supervisorArgv is the supervisor's command line built from the launcher's
// (os.Args[1:]): the same work flags, with --supervise in place of --background.
func supervisorArgv(invocation []string) []string {
	return append(stripWorkModeFlags(invocation), "--"+superviseFlag)
}

// supervisedWorkerArgv is the worker's command line built from the supervisor's.
func supervisedWorkerArgv(invocation []string, supervisorPID int, lockPath string) []string {
	return append(stripWorkModeFlags(invocation),
		"--"+supervisorPIDFlag+"="+strconv.Itoa(supervisorPID),
		"--"+supervisorLockFlag+"="+lockPath)
}

// supervisorLockHolder probes the supervisor lock named by --supervisor-lock
// without taking it. A test seam.
var supervisorLockHolder = func() (bool, int) {
	if workSupervisorLock == "" {
		return false, 0
	}
	return worklock.IsHeldFile(workSupervisorLock)
}

// parentPID is a test seam over os.Getppid.
var parentPID = os.Getppid

// supervisedByUserSupervisor reports whether a live per-user supervisor owns
// this worker: --supervisor-pid names our parent AND that process still holds
// the supervisor lock. It is evaluated at restart time, so a supervisor that
// died since startup is never trusted to bring the worker back.
func supervisedByUserSupervisor() bool {
	if workSupervisorPID <= 0 || parentPID() != workSupervisorPID {
		return false
	}
	held, pid := supervisorLockHolder()
	return held && pid == workSupervisorPID
}

// agentUpdateIsService decides whether AGENT_UPDATE restarts after installing.
// It keeps the existing CITADEL_SERVICE contract and adds the per-user
// supervisor, which restarts the worker on the update exit code.
func agentUpdateIsService() bool {
	return os.Getenv("CITADEL_SERVICE") == "true" || supervisedByUserSupervisor()
}

// userWorkerNodeDir resolves the node config dir for the per-user worker and
// refuses one outside the user's profile (a test seam).
var userWorkerNodeDir = func() (string, error) {
	dir := network.GetNodeConfigDir()
	if err := service.CheckUserWorkerDir(dir, service.UserProfileDirs()); err != nil {
		return "", fmt.Errorf("%w (%s)", err, dir)
	}
	return dir, nil
}

// backgroundLogPath is the launcher's and supervisor's output file.
func backgroundLogPath(nodeConfigDir string) string {
	return filepath.Join(nodeConfigDir, "logs", backgroundLogName)
}

// openBackgroundLog opens the background log for append, first moving a log
// over backgroundLogMaxBytes aside to <name>.1 so it cannot grow without bound.
func openBackgroundLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > backgroundLogMaxBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// runWorkLauncher starts the supervisor hidden and detached and returns its
// exit code. A supervisor that is already running is success.
func runWorkLauncher() int {
	nodeDir, err := userWorkerNodeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	lockPath := service.SupervisorLockPath(nodeDir)
	if held, pid := worklock.IsHeldFile(lockPath); held {
		fmt.Printf("The background worker is already running (supervisor PID %d). Stop it with 'citadel worker stop'.\n", pid)
		return 0
	}
	exe, err := citadelExecutable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: resolve the citadel executable: %v\n", err)
		return 1
	}
	logPath := backgroundLogPath(nodeDir)
	logFile, err := openBackgroundLog(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: open %s: %v\n", logPath, err)
		return 1
	}
	pid, err := startDetachedSupervisor(exe, supervisorArgv(os.Args[1:]), logFile)
	_ = logFile.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: start the background worker: %v\n", err)
		return 1
	}
	// Report once the supervisor holds its lock, or give up quietly: a second
	// supervisor that lost a race exits 0 on its own.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if held, holder := worklock.IsHeldFile(lockPath); held {
			fmt.Printf("Started the background worker (supervisor PID %d). Log: %s\n", holder, logPath)
			return 0
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("Started the background worker (PID %d); it has not reported in yet. Log: %s\n", pid, logPath)
	return 0
}

// acquireSupervisorLock takes the supervisor lock, retrying for up to retry on
// contention so a handover successor can wait for its predecessor.
func acquireSupervisorLock(path string, retry time.Duration) (*worklock.Lock, error) {
	deadline := time.Now().Add(retry)
	for {
		lock, err := worklock.AcquireFile(path, Version, nil)
		var running *worklock.ErrAlreadyRunning
		if err == nil || !errors.As(err, &running) || !time.Now().Before(deadline) {
			return lock, err
		}
		time.Sleep(service.DefaultHandoverPoll)
	}
}

// runWorkSupervisor is the detached supervisor process. Its stdout and stderr
// are the background log.
func runWorkSupervisor() int {
	logf := func(format string, args ...any) {
		fmt.Printf("%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	nodeDir, err := userWorkerNodeDir()
	if err != nil {
		logf("supervisor: %v", err)
		return 1
	}
	lockPath := service.SupervisorLockPath(nodeDir)
	lock, err := acquireSupervisorLock(lockPath, supervisorLockRetry)
	if err != nil {
		var running *worklock.ErrAlreadyRunning
		if errors.As(err, &running) {
			logf("supervisor: another supervisor is already running (PID %d); exiting", running.PID)
			return 0
		}
		logf("supervisor: cannot take the supervisor lock: %v", err)
		return 1
	}
	defer func() { lock.Release() }()

	self := os.Getpid()
	// Holding the lock, every request addressed to another PID is stale. One
	// addressed to us (written since we took the lock) is honoured below.
	service.RemoveStaleStopRequests(nodeDir, self)
	stopPath := service.SupervisorStopPath(nodeDir, self)
	defer func() { _ = os.Remove(stopPath) }()

	exe, err := citadelExecutable()
	if err != nil {
		logf("supervisor: resolve the citadel executable: %v", err)
		return 1
	}
	// The path is captured once: after an update's rename swap it names the new
	// binary, which is what every relaunch and a handover successor must start.
	childArgs := supervisedWorkerArgv(os.Args[1:], self, lockPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			logf("supervisor: shutdown signal")
			cancel()
		case <-ctx.Done():
		}
	}()
	go watchStopFile(ctx, stopPath, supervisorStopPoll, func() {
		logf("supervisor: stop requested")
		cancel()
	})

	handover := service.SupervisorHandover{
		Spawn: func() (int, error) {
			return startDetachedSupervisor(exe, supervisorArgv(os.Args[1:]), os.Stdout)
		},
		Release: func() { lock.Release() },
		Reacquire: func() (int, error) {
			l, err := worklock.AcquireFile(lockPath, Version, nil)
			var running *worklock.ErrAlreadyRunning
			if errors.As(err, &running) {
				return running.PID, nil
			}
			if err != nil {
				return 0, err
			}
			lock = l
			return 0, nil
		},
		Holder:  func() (bool, int) { return worklock.IsHeldFile(lockPath) },
		SelfPID: self,
		Log:     logf,
	}

	logf("supervisor: started (PID %d, %s); worker: %s %s", self, Version, exe, strings.Join(childArgs, " "))
	err = service.RunSupervisor(ctx, service.SupervisorConfig{
		Start: func() (service.SupervisedChild, error) {
			return startSupervisedWorker(exe, childArgs)
		},
		Log:             logf,
		RestartExitCode: update.SupervisorRestartExitCode,
		Handover:        handover.Run,
	})
	if err != nil {
		logf("supervisor: %v", err)
		return 1
	}
	logf("supervisor: stopped")
	return 0
}

// watchStopFile calls onStop once when path appears, polling every interval,
// and returns when ctx ends.
func watchStopFile(ctx context.Context, path string, interval time.Duration, onStop func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			onStop()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
