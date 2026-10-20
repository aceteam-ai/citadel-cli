//go:build windows

package config

import "os"

// keepOwner is a no-op on Windows, where the file ACL is inherited from the
// config directory.
func keepOwner(*os.File, string) error { return nil }
