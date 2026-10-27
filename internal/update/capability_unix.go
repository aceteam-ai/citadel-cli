//go:build linux || darwin

package update

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

func unixUserLabel(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if account, err := user.LookupId(id); err == nil {
		return account.Username + " (uid " + id + ")"
	}
	return "uid " + id
}

func inspectSelfUpdatePath(destination string) SelfUpdateCapability {
	c := SelfUpdateCapability{State: SelfUpdateUnknown, Directory: filepath.Dir(destination), WorkerUser: unixUserLabel(uint32(os.Geteuid()))}
	identity, err := statInstalledIdentity(destination)
	if err != nil {
		return c
	}
	c.BinaryOwner = unixUserLabel(identity.uid)
	lock, err := openMutationLock(destination+".citadel-update.lock", identity, false)
	if err == nil {
		_ = lock.Close()
	} else if !os.IsNotExist(err) {
		if errors.Is(err, os.ErrPermission) {
			c.State = SelfUpdatePrivileged
		}
		return c
	} else if !lockCreationOwnerAllowed(os.Geteuid(), identity.uid) {
		c.State = SelfUpdatePrivileged
		return c
	}
	probe, path, err := createSwapStage(destination, identity)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			c.State = SelfUpdatePrivileged
		}
		return c
	}
	removeOwnedStage(probe, path)
	if err := probe.Close(); err != nil {
		return c
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return c
	}
	c.State = SelfUpdateAvailable
	return c
}
