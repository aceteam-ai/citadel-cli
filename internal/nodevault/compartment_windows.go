//go:build windows

package nodevault

import "os"

func openNoFollow(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDONLY, 0) }

func isSymlinkOpenError(error) bool { return false }

// Windows has no numeric owner here; the key lives in the user's own profile.
func ownedByCurrentUser(os.FileInfo) bool { return true }
