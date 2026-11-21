//go:build windows

package hpc

import "os"

// ownerUID has no numeric equivalent on Windows; the profile refuses there.
func ownerUID(os.FileInfo) (int, error) {
	return -1, ErrUnsupportedPlatform
}

// openNoFollow is not supported on Windows; the profile refuses there.
func openNoFollow(string) (*os.File, error) { return nil, ErrUnsupportedPlatform }

func isSymlinkOpenError(error) bool { return false }
