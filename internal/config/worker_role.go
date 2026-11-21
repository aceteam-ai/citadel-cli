package config

import "fmt"

// WorkerRole is local scheduling policy, not a grant of execution authority.
type WorkerRole string

const (
	WorkerRoleFull    WorkerRole = "full"
	WorkerRoleControl WorkerRole = "control"
)

// ResolveWorkerRole keeps legacy configs full and refuses unknown policies.
func ResolveWorkerRole(value WorkerRole) (WorkerRole, error) {
	switch value {
	case "", WorkerRoleFull:
		return WorkerRoleFull, nil
	case WorkerRoleControl:
		return value, nil
	default:
		return "", fmt.Errorf("worker role must be full or control")
	}
}

// Report omits the legacy/default policy to preserve existing wire payloads.
func (r WorkerRole) Report() string {
	if r == WorkerRoleControl {
		return string(r)
	}
	return ""
}
