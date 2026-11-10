//go:build !windows

package hpc

import (
	"fmt"
	"os"
	"syscall"
)

// ownerUID returns the numeric owner of a file from its stat result.
func ownerUID(info os.FileInfo) (int, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, fmt.Errorf("hpc profile: cannot read owner of %s", info.Name())
	}
	return int(st.Uid), nil
}
