//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

func appliancePathOwnedByCaller(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
