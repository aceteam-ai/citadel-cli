//go:build windows
// +build windows

package platform

import (
	"golang.org/x/sys/windows"
)

// isWindowsAdmin checks if the current process has Administrator privileges on Windows
func isWindowsAdmin() bool {
	var sid *windows.SID

	// Get the built-in Administrators group SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)

	// Check if the current token is a member of the Administrators group
	token := windows.GetCurrentProcessToken()
	member, err := token.IsMember(sid)
	if err != nil {
		return false
	}

	return member
}
