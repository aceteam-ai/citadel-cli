//go:build windows

package localcobrowse

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// acquireOSLock creates a named mutex in the Local\ (logon-session) namespace,
// keyed by the user's SID because Local\ alone is per session, not per user.
// Existence of the object means another live process holds a handle; the
// kernel closes it when that process exits.
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
