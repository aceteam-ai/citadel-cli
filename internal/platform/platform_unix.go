//go:build !windows
// +build !windows

package platform

// isWindowsAdmin is a stub for non-Windows platforms
// This function will never be called on Unix-like systems, but is needed for compilation
func isWindowsAdmin() bool {
	return false
}
