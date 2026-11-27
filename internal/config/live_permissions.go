package config

import (
	"context"
	"time"
)

// PermissionSource returns a fresh, strictly decoded local policy.
type PermissionSource func() (*Permissions, error)

// CapabilityEnabled reports whether a policy permits a routed capability.
func CapabilityEnabled(p *Permissions, category string) bool {
	if p == nil {
		return false
	}
	switch category {
	case "console":
		return p.Console
	case "desktop":
		return p.Desktop
	case "files":
		return p.Files
	case "services":
		return p.Services
	case "ssh":
		return p.SSH
	case "provision":
		return p.Provision
	case "shell":
		return p.Shell
	default:
		return true
	}
}

// StillPermitted checks an admitted operation without re-submitting its passcode
// to the attempt limiter. Sensitive connections also end on passcode rotation.
func (source PermissionSource) StillPermitted(category, admittedHash string) bool {
	p, err := source()
	return err == nil && CapabilityEnabled(p, category) &&
		(!IsSensitiveCategory(category) || p.PasscodeHash == admittedHash)
}

// Watch revokes an attached operation when its local policy changes. Cancellation
// ends the watcher; callers own and close their operation's resources.
func (source PermissionSource) Watch(ctx context.Context, category, admittedHash string, revoke func()) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if !source.StillPermitted(category, admittedHash) {
			revoke()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
