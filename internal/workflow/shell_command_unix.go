//go:build linux || darwin

package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

func newWorkflowShellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Keep CommandContext's refusal to start an already-cancelled context,
	// but disable its watcher: it stops after the leader is reaped, before
	// inherited output pipes necessarily reach EOF. Our helper owns the watch
	// through the whole Cmd.Wait instead of racing a second group signaler.
	cmd.Cancel = nil
	return cmd
}

func workflowShellCombinedOutput(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, errors.New("workflow shell output already configured")
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	err := awaitWorkflowShell(ctx, waited, func() error {
		// A surviving ordinary child retains this dedicated group even after
		// /bin/sh is reaped. Escaped descendants and numeric PGID reuse after
		// the original group disappears are not contained by this mechanism.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
	// Receiving Wait's result joins both process reaping and output copying.
	// No cancellation watcher can signal the group after this helper returns.
	return output.Bytes(), err
}

func awaitWorkflowShell(ctx context.Context, waited <-chan error, killGroup func() error) error {
	select {
	case err := <-waited:
		return err
	default:
	}
	select {
	case err := <-waited:
		return err
	case <-ctx.Done():
		// Prefer an already-complete Wait over a late cancellation request.
		select {
		case err := <-waited:
			return err
		default:
		}
		killErr := killGroup()
		waitErr := <-waited
		if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
			return fmt.Errorf("cancel workflow shell process group: %w", killErr)
		}
		return waitErr
	}
}
