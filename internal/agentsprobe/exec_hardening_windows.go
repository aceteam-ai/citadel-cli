//go:build windows

// internal/agentsprobe/exec_hardening_windows.go
//
// Windows stub for the `--version` exec hardening. There is no privilege-drop
// model here (SysProcAttr's shape differs; no syscall.Credential), and the
// Windows worker probes its OWN account (Target.Signal == process, cred is always
// nil), so there is nothing to drop and no process-group kill to install.
package agentsprobe

import "os/exec"

func applyExecHardening(_ *exec.Cmd, _ *Credential) {}
