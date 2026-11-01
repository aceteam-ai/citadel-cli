//go:build windows

package update

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func inspectSelfUpdatePath(destination string) SelfUpdateCapability {
	c := SelfUpdateCapability{State: SelfUpdateUnknown, Directory: filepath.Dir(destination)}
	identity, err := windowsInstalledIdentity(destination)
	if err != nil {
		return c
	}
	c.BinaryOwner = identity.owner.String()
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return c
	}
	defer token.Close()
	actor, err := token.GetTokenUser()
	if err != nil {
		return c
	}
	c.WorkerUser = actor.User.Sid.String()
	lock, err := openWindowsMutationLock(destination+".citadel-update.lock", identity, false)
	if err == nil {
		_ = windows.CloseHandle(lock)
	} else if !windowsPathAbsent(err) {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			c.State = SelfUpdatePrivileged
		}
		return c
	} else if !actor.User.Sid.Equals(identity.owner) {
		c.State = SelfUpdatePrivileged
		return c
	}
	probe, path, err := createSwapStage(destination, identity)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || errors.Is(err, windows.ERROR_ACCESS_DENIED) || !actor.User.Sid.Equals(identity.owner) {
			c.State = SelfUpdatePrivileged
		}
		return c
	}
	// Windows does not allow removing the probe while it is held open.
	if err := probe.Close(); err != nil {
		return c
	}
	if err := os.Remove(path); err != nil {
		return c
	}
	c.State = SelfUpdateAvailable
	return c
}
