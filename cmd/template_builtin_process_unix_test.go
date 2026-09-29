//go:build !windows

package cmd

import (
	"os/exec"
	"testing"
)

func TestConfigurePapercraftProcessTreeUnix(t *testing.T) {
	cmd := exec.Command("unused")
	configurePapercraftProcessTree(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Chromium must launch in a dedicated process group")
	}
	if cmd.Cancel == nil {
		t.Fatal("Chromium process group cancellation is not configured")
	}
}
