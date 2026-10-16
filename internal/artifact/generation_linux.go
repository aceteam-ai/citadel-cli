//go:build linux

package artifact

import (
	"fmt"
	"os"
	"syscall"
)

func Supported() bool { return true }

func observe(info os.FileInfo) (Identity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxSafeInteger {
		return Identity{}, fmt.Errorf("required Linux regular-file observations unavailable")
	}
	return Identity{Bytes: info.Size(), Device: uint64(stat.Dev), Inode: uint64(stat.Ino),
		MtimeSeconds: int64(stat.Mtim.Sec), MtimeNanoseconds: int64(stat.Mtim.Nsec),
		CtimeSeconds: int64(stat.Ctim.Sec), CtimeNanoseconds: int64(stat.Ctim.Nsec)}, nil
}
