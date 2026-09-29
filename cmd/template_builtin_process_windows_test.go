//go:build windows

package cmd

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestConfigurePapercraftProcessTreeWindows(t *testing.T) {
	cmd := exec.Command("unused.exe")
	configurePapercraftProcessTree(cmd)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("Chromium must launch in a dedicated Windows process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("Chromium taskkill tree cancellation is not configured")
	}
}
