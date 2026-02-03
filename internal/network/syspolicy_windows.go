//go:build windows

package network

import "tailscale.com/util/winutil/gp"

// restrictPolicyLocks prevents Group Policy lock acquisition during tsnet startup.
// On Windows, syspolicy tries to acquire a GP read lock which fails with
// ERROR_ACCESS_DENIED in non-interactive sessions (WinRM, services).
// Returns a function to lift the restriction after startup completes.
func restrictPolicyLocks() (removeRestriction func()) {
	return gp.RestrictPolicyLocks()
}
