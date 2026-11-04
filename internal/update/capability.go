package update

import (
	"fmt"
	"path/filepath"
	"runtime"
)

const (
	SelfUpdateAvailable  = "available"
	SelfUpdatePrivileged = "requires_privileged_install"
	SelfUpdateUnknown    = "unknown"
)

// SelfUpdateCapability describes the current process's install authority. It is
// an observation, never a grant: apply still rechecks the lock and file identity.
type SelfUpdateCapability struct {
	State       string `json:"state"`
	Directory   string `json:"directory,omitempty"`
	BinaryOwner string `json:"binary_owner,omitempty"`
	WorkerUser  string `json:"worker_user,omitempty"`
}

func (c SelfUpdateCapability) Message() string {
	return fmt.Sprintf("self-update %s: binary owner %q, worker user %q, install directory %q; update this node with its binary owner's privileged installer, or explicitly configure a self-updating install owned by the worker user", c.State, c.BinaryOwner, c.WorkerUser, c.Directory)
}

// InspectSelfUpdateCapability creates and removes only a private same-directory
// probe. It neither provisions a lock nor changes the installed binary/owner.
// A deliberately root-owned install remains supported and is reported as a
// capability requiring privileged installation, even with auto-update disabled.
func InspectSelfUpdateCapability() SelfUpdateCapability {
	destination, err := resolveInstalledPath()
	if err != nil {
		return SelfUpdateCapability{State: SelfUpdateUnknown}
	}
	if IsHomebrewManagedPath(destination, runtime.GOOS) {
		return SelfUpdateCapability{State: "homebrew_managed", Directory: filepath.Dir(destination)}
	}
	if IsDesktopManagedPath(destination, runtime.GOOS) {
		return SelfUpdateCapability{State: "desktop_managed", Directory: filepath.Dir(destination)}
	}
	return inspectSelfUpdatePath(destination)
}
