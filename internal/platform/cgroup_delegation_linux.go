//go:build linux

package platform

import (
	"fmt"
	"os"
)

// CheckRootlessCgroupDelegation checks the systemd user-service delegation
// boundary Podman uses for rootless resource limits.
func CheckRootlessCgroupDelegation(required []string) CgroupDelegationHealth {
	path := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", os.Getuid(), os.Getuid())
	content, err := os.ReadFile(path)
	return evaluateCgroupDelegation(required, path, content, err)
}
