//go:build !linux

package service

// RematerializeManagedUnits is a no-op on non-Linux platforms. The #444
// restart-storm hardening it re-materializes is expressed in systemd directives
// (StartLimit*/Restart*), which only apply to the Linux systemd units. launchd
// (KeepAlive) and the Windows SCM (sc failure) express their own restart policy
// set at install time and carry no equivalent directives to refresh here.
func RematerializeManagedUnits(logf func(format string, args ...any)) ([]string, error) {
	return nil, nil
}
