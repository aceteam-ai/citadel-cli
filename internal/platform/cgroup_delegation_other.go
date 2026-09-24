//go:build !linux

package platform

// CheckRootlessCgroupDelegation is not applicable outside Linux.
func CheckRootlessCgroupDelegation(required []string) CgroupDelegationHealth {
	return CgroupDelegationHealth{OK: true, Message: "rootless cgroup delegation is only applicable on Linux"}
}
