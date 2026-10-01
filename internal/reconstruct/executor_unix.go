//go:build !windows

package reconstruct

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

type osExecutor struct{}

// NewOSExecutor returns the Linux/Unix process executor. Every command starts
// in its own process group; context cancellation kills the entire group rather
// than only ffmpeg or Spirula's direct parent process.
func NewOSExecutor() Executor { return osExecutor{} }

func (osExecutor) Run(ctx context.Context, invocation Invocation) error {
	cmd := newOSCommand(ctx, invocation)
	err := cmd.Run()
	if contextErr := ctx.Err(); contextErr != nil {
		return errors.Join(contextErr, err)
	}
	return err
}

func newOSCommand(ctx context.Context, invocation Invocation) *exec.Cmd {
	cmd := exec.CommandContext(ctx, invocation.Name, invocation.Args...)
	cmd.Dir = invocation.Dir
	cmd.Stdout = invocation.Stdout
	cmd.Stderr = invocation.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// If a broken child ignores teardown or leaves pipes open, os/exec applies
	// its direct-child fallback after the process-group cancel above.
	cmd.WaitDelay = 10 * time.Second
	return cmd
}
