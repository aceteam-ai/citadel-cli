//go:build !windows

// internal/agentsprobe/exec_hardening_unix.go
//
// POSIX half of the `--version` exec hardening (aceteam #8993 S2). Split behind a
// build tag because syscall.SysProcAttr's Credential/Setpgid fields and
// syscall.Kill do not exist in this shape on Windows (see exec_hardening_windows.go).
package agentsprobe

import (
	"errors"
	"os/exec"
	"syscall"
)

// applyExecHardening sets Setpgid so a `--version` that forks a daemonizing
// grandchild lands in its own process group, and a Cancel that SIGKILLs that
// whole group on ctx timeout so the grandchild is reaped with the parent
// (exec.CommandContext alone kills only the direct child). When cred != nil it
// ALSO drops privileges: the child runs as cred's uid/gid.
//
// Groups is left nil with NoSetGroups unset, so the runtime calls
// setgroups(0, NULL) before setgid/setuid -- clearing the worker's supplementary
// groups from the child (the tightest drop). The root worker must never exec a
// target-user-writable binary with root's groups.
func applyExecHardening(cmd *exec.Cmd, cred *Credential) {
	sp := &syscall.SysProcAttr{Setpgid: true}
	if cred != nil {
		sp.Credential = &syscall.Credential{
			Uid: uint32(cred.UID),
			Gid: uint32(cred.GID),
		}
	}
	cmd.SysProcAttr = sp
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the whole process group (Setpgid put the child at
		// the head of a new group whose pgid == its pid). ESRCH -> the group
		// already exited, the common race; treat as success.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
