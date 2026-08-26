// internal/servicediag/managed.go
package servicediag

import "github.com/aceteam-ai/citadel-cli/services"

// IsManaged reports whether name is a MANAGED service: present in the node's
// citadel.yaml `services:` list (source "manifest") or in the embedded
// catalog (source "catalog") -- as opposed to an ad-hoc/unmanaged container
// diagnose has no business reasoning about (citadel #852's explicit scope
// limit). manifestServiceNames is every service name declared in the node's
// manifest (empty/nil when no manifest could be read).
func IsManaged(name string, manifestServiceNames []string) (managed bool, source string) {
	for _, n := range manifestServiceNames {
		if n == name {
			return true, "manifest"
		}
	}
	if _, ok := services.ServiceMap[name]; ok {
		return true, "catalog"
	}
	return false, ""
}
