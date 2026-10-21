//go:build !linux

package mediainput

import "os"

// Edit acquisition is Linux-only in v1. Do not silently fall back to path-based
// opens or weaken the no-symlink primitive on another platform.
func openWorkspaceInput(*os.Root, string) (*os.File, error) { return nil, os.ErrInvalid }
