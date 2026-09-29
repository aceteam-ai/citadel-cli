//go:build windows

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// configurePapercraftProcessTree gives Chromium a new process group and uses
// taskkill's tree mode on cancellation. A direct Process.Kill fallback still
// guarantees termination when taskkill is unavailable.
func configurePapercraftProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if taskkill, err := exec.LookPath("taskkill.exe"); err == nil {
			err = exec.Command(taskkill, "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
			if err == nil {
				return nil
			}
		}
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		return nil
	}
}
