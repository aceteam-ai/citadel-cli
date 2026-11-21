//go:build !windows

package platform

import "syscall"

// reapChild collects an exited child so it does not linger as a zombie. After a
// restart by exec the adopted browser is still this process's child. ECHILD
// (not our child, or already reaped) is ignored.
func reapChild(pid int) { _, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil) }
