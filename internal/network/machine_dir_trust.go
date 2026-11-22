// internal/network/machine_dir_trust.go
package network

import (
	"errors"
	"fmt"
	"os"
)

// CheckMachineConfigDirTrusted reports whether the machine-wide config dir
// (where the state-dir pointer and the global config.yaml are read from,
// before any per-user location) can be trusted to have been written by an
// administrator. A missing dir is trusted: nothing is read from it.
//
// On Windows that dir is %ProgramData%\Citadel, which an ordinary user can
// create; a per-user install never does. Another local user could then plant
// a pointer that moves this user's node state (device key, network key) into a
// folder they can read. A dir that exists must therefore be a plain directory
// that only administrators can own or modify (checkDirectoryNotWritableByNonAdmin).
//
// On Linux and macOS the machine dir needs root to create, so this reports
// trusted; callers still check where the resolved node dir lies.
func CheckMachineConfigDirTrusted() error {
	return checkMachineConfigDir(getGlobalConfigDirForState(), checkMachineDirACL)
}

func checkMachineConfigDir(dir string, acl func(string) error) error {
	if dir == "" {
		return nil
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("machine config dir %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return fmt.Errorf("machine config dir %s is not a plain directory", dir)
	}
	if acl == nil {
		return nil
	}
	return acl(dir)
}
