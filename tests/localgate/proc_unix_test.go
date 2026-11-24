//go:build !windows

package localgate

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own group so killTree can reach the
// browser it launched.
func setProcessGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killTree(c *exec.Cmd) { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
