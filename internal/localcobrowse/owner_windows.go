//go:build windows

package localcobrowse

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// acquireOSLock creates a named mutex in the Local\ namespace, which is
// scoped to the logon session; the user's SID in the name keeps two users
// apart within a session. So this is one owner per user per logon session,
// which is the same thing for an interactive desktop user. Existence of the
// object means another live process holds a handle; the kernel closes it when
// that process exits.
func acquireOSLock(dir string) (func(), error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current user: %w", err)
	}
	name, err := windows.UTF16PtrFromString(`Local\citadel-local-cobrowse-` + u.User.Sid.String())
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			_ = windows.CloseHandle(h)
		}
		return nil, ownerErrorFrom(dir)
	}
	if err != nil {
		return nil, fmt.Errorf("create owner mutex: %w", err)
	}
	return func() { _ = windows.CloseHandle(h) }, nil
}
