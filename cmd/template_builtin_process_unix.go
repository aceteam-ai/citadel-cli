//go:build !windows

package cmd

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
)

// configurePapercraftProcessTree combines a process-group kill with the
// transient systemd scope stop owned by newRenderLimitedCommand. The process
// group is a fast local fallback; the scope is authoritative for descendants
// that call setsid and escape their ancestor's pgid.
func configurePapercraftProcessTree(limited *renderCommand) func() error {
	cmd := limited.cmd
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
			scopeErr := limited.stopSystemdScope()
			var groupErr error
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				groupErr = fmt.Errorf("kill render process group: %w", err)
			}
			stopErr = errors.Join(scopeErr, groupErr)
		})
		return stopErr
	}
	cmd.Cancel = terminate
	return terminate
}
