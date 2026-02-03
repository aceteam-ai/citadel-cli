//go:build !windows

package network

func restrictPolicyLocks() (removeRestriction func()) {
	return func() {}
}
