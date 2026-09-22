package platform

import (
	"fmt"
	"sort"
	"strings"
)

// CgroupDelegationHealth reports whether a rootless user service owns every
// cgroup v2 controller needed to enforce a requested container limit.
type CgroupDelegationHealth struct {
	Applicable  bool
	OK          bool
	Path        string
	Controllers []string
	Missing     []string
	Message     string
	Hint        string
}

func (h CgroupDelegationHealth) String() string {
	if h.Message != "" {
		return h.Message
	}
	if !h.Applicable {
		return "not applicable"
	}
	if h.OK {
		return "delegated controllers: " + strings.Join(h.Controllers, ", ")
	}
	return "missing delegated controllers: " + strings.Join(h.Missing, ", ")
}

func normalizeControllers(controllers []string) []string {
	seen := make(map[string]bool, len(controllers))
	for _, controller := range controllers {
		controller = strings.TrimSpace(controller)
		if controller != "" {
			seen[controller] = true
		}
	}
	out := make([]string, 0, len(seen))
	for controller := range seen {
		out = append(out, controller)
	}
	sort.Strings(out)
	return out
}

func evaluateCgroupDelegation(required []string, path string, content []byte, readErr error) CgroupDelegationHealth {
	required = normalizeControllers(required)
	health := CgroupDelegationHealth{Applicable: true, Path: path}
	if readErr != nil {
		health.Missing = required
		health.Message = fmt.Sprintf("cannot read rootless cgroup delegation at %s: %v", path, readErr)
		health.Hint = "configure systemd Delegate=cpu memory pids for the user service"
		return health
	}
	health.Controllers = normalizeControllers(strings.Fields(string(content)))
	available := make(map[string]bool, len(health.Controllers))
	for _, controller := range health.Controllers {
		available[controller] = true
	}
	for _, controller := range required {
		if !available[controller] {
			health.Missing = append(health.Missing, controller)
		}
	}
	health.OK = len(health.Missing) == 0
	if !health.OK {
		health.Message = "rootless Podman cannot enforce requested limits; missing delegated controllers: " + strings.Join(health.Missing, ", ")
		health.Hint = "configure systemd Delegate=cpu memory pids for the user service"
	}
	return health
}
