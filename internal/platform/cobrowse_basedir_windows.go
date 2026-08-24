//go:build windows

// internal/platform/cobrowse_basedir_windows.go
//
// Windows counterpart of cobrowse_basedir_unix.go. Co-browse sessions are a
// Linux-first node feature (headless Chromium on a managed Xvfb display; Windows has
// no Xvfb, and ChromiumAvailable/XvfbAvailable already gate the feature off there in
// practice). Go's os.FileMode on Windows does not carry unix-style owner/group/other
// write bits, and ownership is ACL-based rather than a single UID, so the unix
// mode+UID check does not translate. This stub keeps the package compiling for
// GOOS=windows without pretending to enforce a check that has no Windows equivalent.
package platform

import "os"

func validateCobrowseBaseDirPerms(dir string, info os.FileInfo) error {
	return nil
}
