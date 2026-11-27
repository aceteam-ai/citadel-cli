//go:build linux

package cmd

import (
	"os"
	"syscall"
)

// statFileStamp returns the stamp for fi, or ok=false when the platform cannot
// supply file identity and ctime (callers must then not cache).
func statFileStamp(fi os.FileInfo) (fileStamp, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStamp{}, false
	}
	return fileStamp{
		dev: uint64(st.Dev), ino: uint64(st.Ino),
		mtime: st.Mtim.Nano(), ctime: st.Ctim.Nano(), size: st.Size,
	}, true
}
