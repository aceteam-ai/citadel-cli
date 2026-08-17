//go:build windows

// internal/network/wintun_hook_windows.go
package network

// ensureWintunDriverIfNeeded prepares the embedded wintun driver on Windows
// (issue #709). See EnsureWintunDriver for the extraction/verification/load
// sequence; this indirection exists so machinewide.go, which is built on
// every platform, can call one name that is a real check on Windows and a
// no-op everywhere else (wintun_hook_other.go).
func ensureWintunDriverIfNeeded() error {
	return EnsureWintunDriver()
}
