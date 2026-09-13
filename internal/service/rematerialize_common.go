// internal/service/rematerialize_common.go
//
// Platform-independent helpers shared by the systemd (linux) and launchd
// (darwin) re-materialization paths: back up a managed unit/plist before
// rewriting it, and write the replacement atomically. Kept non-tagged so both
// RematerializeManagedUnits implementations reuse one copy.
package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// backupUnit copies the current unit/plist to <path>.citadel-bak so an operator
// can restore their exact prior file. Best-effort atomicity: the backup is
// written fully before the caller overwrites the original.
func backupUnit(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".citadel-bak", data, 0o644)
}

// writeUnitFile atomically writes file content, preserving 0644 perms. Writes to
// a temp file in the same directory then renames, so a crash mid-write can never
// leave a truncated (unparseable) unit/plist.
func writeUnitFile(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".citadel-unit-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename unit into place: %w", err)
	}
	return nil
}
