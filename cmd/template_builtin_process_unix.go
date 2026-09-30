//go:build !windows

package cmd

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
)

// configurePapercraftProcessTree combines a live-child process-group fallback
// with the transient systemd scope owned by newRenderLimitedCommand. The scope
// is the sole post-Wait authority: once the root has been reaped, its numeric
// PID/PGID can be reused and must never be signaled. CommandContext may use the
// group fallback only while canceling the still-live command, before stopping
// the scope that authoritatively covers setsid descendants.
func configurePapercraftProcessTree(limited *renderCommand) func() error {
	cmd := limited.cmd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var (
		stopOnce sync.Once
		stopErr  error
	)
	terminate := func(killLiveGroup bool) error {
		if cmd.Process == nil {
			return nil
		}
		stopOnce.Do(func() {
			var groupErr error
			if killLiveGroup {
				if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					groupErr = fmt.Errorf("kill live render process group: %w", err)
				}
			}
			scopeErr := limited.stopSystemdScope()
			stopErr = errors.Join(scopeErr, groupErr)
		})
		return stopErr
	}
	cmd.Cancel = func() error { return terminate(true) }
	return func() error { return terminate(false) }
}
