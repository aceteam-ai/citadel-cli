package platform

import (
	"fmt"
	"os"
	"runtime"
)

// ValidatePrivateFile requires an existing regular, owner-only file, with the
// same ownership and Windows protected-DACL rules as the browser session dir.
func ValidatePrivateFile(path string) error { return validatePrivatePath(path, false) }

// ValidatePrivateDir is read-only: unlike EnsurePrivateDir it creates nothing.
// Unix settings dirs must also be unreadable by other users.
func ValidatePrivateDir(path string) error { return validatePrivatePath(path, true) }

func validatePrivatePath(path string, directory bool) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if (directory && !st.IsDir()) || (!directory && !st.Mode().IsRegular()) {
		return fmt.Errorf("not a private regular file or directory")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("path is accessible to other users")
	}
	return validateCobrowseBaseDirPerms(path, st)
}

// SecurePrivateFile is for a newly created temp file in an already trusted
// private directory. Windows needs an explicit protected DACL, not Chmod alone.
func SecurePrivateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := secureNewCobrowseBaseDir(path); err != nil {
		return err
	}
	return ValidatePrivateFile(path)
}
