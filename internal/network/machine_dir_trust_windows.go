//go:build windows

// internal/network/machine_dir_trust_windows.go
package network

// checkMachineDirACL requires an administrator-like owner and no write,
// delete or take-ownership right for any non-administrator.
var checkMachineDirACL = checkDirectoryNotWritableByNonAdmin
