//go:build !linux

// internal/services/external_supervisor_other.go
package services

// externalSupervisorUnit is a no-op off Linux: the systemd-cgroup detection
// reads /proc, which only exists on Linux. macOS/Windows native engines are
// never classified as externally managed, so StopNativeService's behavior there
// is unchanged (a launchd/SCM-owned native engine is a documented follow-up, not
// part of this fix). Kept a var so the signature matches the Linux build.
var externalSupervisorUnit = func(pid int) (unit string, external bool) {
	return "", false
}
