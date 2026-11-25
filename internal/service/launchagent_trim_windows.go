//go:build windows

package service

// truncateIfOversize is a no-op: the LaunchAgent exists only on macOS.
func truncateIfOversize(path string, max int64) {}
