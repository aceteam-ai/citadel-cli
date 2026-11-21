//go:build windows

package update

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// On an elevated runner (GitHub's windows-latest) new files are owned by
// BUILTIN\Administrators, because that is the elevated token's default owner.
// The production first-lock rule requires the binary owner to be the worker
// user, so fixtures must be owned by the current user. The production check is
// untouched; only test fixtures are adjusted.
func init() {
	// Best effort: make files created later by this test process (fixtures and
	// the update code's own stage files) default to the current user.
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &token); err != nil {
		return
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return
	}
	owner := struct{ Owner *windows.SID }{Owner: user.User.Sid} // TOKEN_OWNER
	_ = windows.SetTokenInformation(token, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner)))
}

// ensureFixtureOwnedByCurrentUser sets path's owner to the current user's SID.
func ensureFixtureOwnedByCurrentUser(t *testing.T, path string) {
	t.Helper()
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("read current user: %v", err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read fixture owner: %v", err)
	}
	if owner, _, err := sd.Owner(); err == nil && owner != nil && owner.Equals(user.User.Sid) {
		return
	}
	enableTestPrivilege("SeRestorePrivilege")
	enableTestPrivilege("SeTakeOwnershipPrivilege")
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); err != nil {
		t.Fatalf("set fixture owner to %s: %v", user.User.Sid, err)
	}
}

func enableTestPrivilege(name string) {
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
		return
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return
	}
	defer token.Close()
	privs := windows.Tokenprivileges{PrivilegeCount: 1}
	privs.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	_ = windows.AdjustTokenPrivileges(token, false, &privs, 0, nil, nil)
}
