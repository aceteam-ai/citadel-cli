//go:build windows

package platform

// reapProfileUsers is a no-op on Windows: the kill-on-close Job Object ends
// every browser descendant with the session.
func reapProfileUsers(string, string) error { return nil }
