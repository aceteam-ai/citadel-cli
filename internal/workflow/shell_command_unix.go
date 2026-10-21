//go:build linux || darwin

package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func newWorkflowShellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Ordinary shell descendants share this dedicated group. Killing just
		// /bin/sh leaves a sleeping child alive with the output pipe open.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// No pipe wait timeout stands in for actual exit. Detached children that
	// escape the group are not claimed contained by this mechanism.
	return cmd
}
