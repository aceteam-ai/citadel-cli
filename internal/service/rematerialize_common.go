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

// selfExe returns the absolute path to the currently-running citadel binary, or
// "" if it cannot be resolved. Used to build a remediation command that works
// regardless of how citadel is on PATH (see rootRemediationCommand).
func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// rootRemediationCommand returns the command an operator should run as root to
// apply a refresh that the current (unprivileged) invocation could not
// (citadel-cli#1266). It deliberately avoids a bare `sudo citadel ...`: sudo's
// secure_path strips ~/.local/bin (and other non-standard install locations),
// so a bare `sudo citadel` is "command not found" for the common `~/.local/bin`
// install. An ABSOLUTE path is used instead -- shell-agnostic (works in fish
// too, unlike `$(command -v citadel)`) and immune to secure_path. When
// os.Executable() could not be resolved (exe == ""), it falls back to a
// command-substitution form that still avoids the broken bare invocation.
//
// It targets `service refresh-unit`, not `update install`: refresh-unit is
// network-free, so the remediation cannot fail a second way on a node without
// internet egress (the already-latest path of `update install` still exits
// non-zero when the GitHub check fails).
func rootRemediationCommand(exe string) string {
	if exe != "" {
		// %q double-quotes the path, which is correct for any realistic
		// Linux/macOS binary path (including one with spaces). It is NOT a
		// general shell-escaper -- a path containing $, backtick, or a backslash
		// would be Go-escaped, not shell-escaped -- but such a citadel install
		// path does not occur in practice; do not "fix" this into strconv.Quote.
		return fmt.Sprintf("sudo %q service refresh-unit", exe)
	}
	return `sudo "$(command -v citadel)" service refresh-unit`
}

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
