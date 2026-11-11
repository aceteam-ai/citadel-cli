//go:build !linux

package hpc

// CheckListenerOwner has no /proc to read off Linux. HPC clusters are Linux;
// elsewhere the check is skipped (nil), which only affects development hosts.
func CheckListenerOwner(port, uid int) ListenerCheck { return nil }
