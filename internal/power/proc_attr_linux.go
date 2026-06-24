//go:build linux

package power

import "syscall"

// procSysProcAttr puts the inhibitor child in its own process group (so Stop can
// kill the whole group) and asks the kernel to SIGKILL it if Citadel dies. The
// Pdeathsig watch closes the leak path where Citadel exits via os.Exit, SIGKILL,
// or a crash without running our cleanup: without it, killing systemd-inhibit's
// parent would leave the assertion held until the orphaned process happened to
// exit. (Under systemd the cgroup kill already reaps it, but interactive runs
// rely on this.)
func procSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}
