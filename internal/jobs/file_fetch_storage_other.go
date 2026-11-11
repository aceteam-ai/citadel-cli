//go:build !linux

package jobs

import "os"

// FILE_FETCH is unavailable without the Linux anonymous staging primitive.
func fileFetchOpenStaging(*os.Root) (*os.File, error)   { return nil, os.ErrInvalid }
func fileFetchPublish(*os.Root, *os.File, string) error { return os.ErrInvalid }
func fileFetchStorageSupported(string) bool             { return false }
