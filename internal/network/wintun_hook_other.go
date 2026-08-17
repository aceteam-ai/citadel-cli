//go:build !windows

// internal/network/wintun_hook_other.go
package network

// ensureWintunDriverIfNeeded is a no-op on non-Windows platforms: Linux uses
// /dev/net/tun and macOS uses utun, both kernel-native, with no external
// driver file to extract or verify. See wintun_hook_windows.go.
func ensureWintunDriverIfNeeded() error {
	return nil
}
