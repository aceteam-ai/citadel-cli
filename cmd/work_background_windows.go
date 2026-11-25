//go:build windows

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/service"
	"golang.org/x/sys/windows"
)

// Process creation for the per-user worker.
//
// The supervisor is started with CREATE_NO_WINDOW, not DETACHED_PROCESS:
// Windows ignores CREATE_NO_WINDOW when DETACHED_PROCESS is also set, and a
// console program started by a parent with no console opens a new, visible
// console. With a hidden console of its own, the supervisor's worker, and every
// docker or shell command the worker runs, share that hidden console, so no
// window appears. CREATE_NEW_PROCESS_GROUP detaches it from the launcher's
// Ctrl+C, and CREATE_BREAKAWAY_FROM_JOB (best effort) keeps it alive when the
// launcher runs inside a job that is closed after it exits.
const (
	supervisorCreationFlags = windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP
	// The worker shares the supervisor's hidden console in its own process
	// group, so the supervisor can send it CTRL_BREAK (os.Interrupt) to stop it
	// gracefully.
	workerCreationFlags = windows.CREATE_NEW_PROCESS_GROUP
)

// startDetachedSupervisor starts the supervisor hidden, with its output in
// logFile, and does not wait for it.
func startDetachedSupervisor(exe string, args []string, logFile *os.File) (int, error) {
	start := func(flags uint32) (int, error) {
		c := exec.Command(exe, args...)
		c.Stdin = nil
		c.Stdout = logFile
		c.Stderr = logFile
		c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		if err := c.Start(); err != nil {
			return 0, err
		}
		pid := c.Process.Pid
		_ = c.Process.Release()
		return pid, nil
	}
	pid, err := start(supervisorCreationFlags | windows.CREATE_BREAKAWAY_FROM_JOB)
	if err != nil {
		// A job that forbids breakaway refuses the whole CreateProcess.
		pid, err = start(supervisorCreationFlags)
	}
	return pid, err
}

// windowsSupervisedWorker is the running worker under the supervisor, inside a
// kill-on-close job so it can never outlive the supervisor.
type windowsSupervisedWorker struct {
	cmd  *exec.Cmd
	job  windows.Handle
	done chan struct{}
	code int

	// mu guards job against Kill racing the close after the worker exits.
	mu sync.Mutex
}

// startSupervisedWorker starts one worker for the supervisor. Its output goes
// to the supervisor's own stdout and stderr (the background log).
func startSupervisedWorker(exe string, args []string) (service.SupervisedChild, error) {
	c := exec.Command(exe, args...)
	c.Stdin = nil
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: workerCreationFlags}
	job, err := platform.StartInKillOnCloseJob(c)
	if err != nil {
		return nil, err
	}
	w := &windowsSupervisedWorker{cmd: c, job: job, done: make(chan struct{}), code: -1}
	go func() {
		err := c.Wait()
		w.code = exitCodeOf(c, err)
		w.closeJob()
		close(w.done)
	}()
	return w, nil
}

func (w *windowsSupervisedWorker) Done() <-chan struct{} { return w.done }

// ExitCode is only read after Done is closed, which orders it after the write.
func (w *windowsSupervisedWorker) ExitCode() int { return w.code }

// Interrupt sends CTRL_BREAK to the worker's process group, which Go delivers
// as os.Interrupt: the worker's normal graceful shutdown.
func (w *windowsSupervisedWorker) Interrupt() error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(w.cmd.Process.Pid))
}

// Kill terminates the worker and everything it started.
func (w *windowsSupervisedWorker) Kill() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.job == 0 {
		return nil // already exited and reaped
	}
	return windows.TerminateJobObject(w.job, 1)
}

// closeJob closes the job once the worker has exited, which also ends anything
// it left running.
func (w *windowsSupervisedWorker) closeJob() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.job != 0 {
		_ = windows.CloseHandle(w.job)
		w.job = 0
	}
}

func exitCodeOf(c *exec.Cmd, err error) int {
	if c.ProcessState != nil {
		return c.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
