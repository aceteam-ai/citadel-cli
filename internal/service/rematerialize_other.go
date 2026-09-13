//go:build !linux && !darwin

package service

// RematerializeManagedUnits is a no-op on platforms other than Linux and macOS
// (i.e. Windows and anything else). The Windows SCM expresses its restart policy
// (sc failure) at install time and carries no on-disk directives to refresh
// here. Linux (systemd hardening) and macOS (launchd Homebrew ExecPath drift)
// have their own implementations in rematerialize.go / rematerialize_darwin.go.
func RematerializeManagedUnits(logf func(format string, args ...any)) ([]string, error) {
	return nil, nil
}
