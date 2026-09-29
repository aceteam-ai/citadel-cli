//go:build !windows

package cmd

import (
	"errors"
	"os/exec"
	"syscall"
)

// configurePapercraftProcessTree places Chromium and every renderer/helper it
// forks in a dedicated process group. CommandContext cancellation then kills
// the whole group before Wait returns, rather than leaving browser children
// holding the temporary profile open.
func configurePapercraftProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
