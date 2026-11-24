//go:build !linux && !darwin

package cmd

import "os"

// statFileStamp reports ok=false: without file identity and ctime the
// allowlist is re-read on every call rather than cached.
func statFileStamp(os.FileInfo) (fileStamp, bool) { return fileStamp{}, false }
