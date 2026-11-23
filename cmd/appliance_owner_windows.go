//go:build windows

package cmd

import "os"

// Windows enforces the existing configuration ACL through the file operations;
// POSIX sudo/uid ownership does not apply. Symlinks remain refused by the caller.
func appliancePathOwnedByCaller(os.FileInfo) bool { return true }
