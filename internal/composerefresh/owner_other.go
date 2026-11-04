//go:build !unix

package composerefresh

import "os"

func keepComposeOwner(*os.File, string) error { return nil }
