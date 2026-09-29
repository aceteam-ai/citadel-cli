//go:build !windows

package cmd

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

// configurePapercraftProcessTree places Chromium and every renderer/helper it
// forks in a dedicated process group. CommandContext cancellation then kills
// the whole group before Wait returns, rather than leaving browser children
// holding the temporary profile open.
func configurePapercraftProcessTree(cmd *exec.Cmd) func() error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var (
		stopOnce sync.Once
		stopErr  error
	)
	terminate := func() error {
		if cmd.Process == nil {
			return nil
		}
		stopOnce.Do(func() {
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				stopErr = err
			}
		})
		return stopErr
	}
	cmd.Cancel = terminate
	return terminate
}
