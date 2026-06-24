//go:build darwin

package power

import "syscall"

// procSysProcAttr puts the inhibitor child in its own process group so Stop can
// kill the whole group. darwin's syscall.SysProcAttr has no Pdeathsig field, so
// the darwin inhibitor instead relies on caffeinate's `-w <pid>` watch to tear
// the assertion down if Citadel exits without running cleanup (see
// power_darwin.go's NewInhibitor).
func procSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
