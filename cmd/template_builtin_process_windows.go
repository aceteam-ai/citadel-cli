//go:build windows

package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

// configurePapercraftProcessTree gives Chromium a new process group and uses
// taskkill's tree mode on cancellation. A direct Process.Kill fallback stops
// the root process, but cannot prove that its descendants exited, so that path
// reports an incomplete shutdown and prevents profile removal.
func configurePapercraftProcessTree(cmd *exec.Cmd) func() error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	var (
		stopOnce sync.Once
		stopErr  error
	)
	terminate := func() error {
		if cmd.Process == nil {
			return nil
		}
		stopOnce.Do(func() {
			taskkill, lookupErr := exec.LookPath("taskkill.exe")
			if lookupErr == nil {
				if err := exec.Command(taskkill, "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run(); err == nil {
					return
				} else {
					lookupErr = err
				}
			}
			killErr := cmd.Process.Kill()
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
			stopErr = errors.Join(
				errChromiumShutdownIncomplete,
				fmt.Errorf("terminate Chromium process tree with taskkill: %w", lookupErr),
				killErr,
			)
		})
		return stopErr
	}
	cmd.Cancel = terminate
	return terminate
}
