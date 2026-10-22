//go:build !windows

package worklock

import (
	"errors"
	"os"
	"syscall"
)

func nativeContentionProbeOps() contentionProbeOps {
	return contentionProbeOps{
		lstat: os.Lstat,
		open: func(path string) (contentionProbeFile, error) {
			// NONBLOCK prevents a FIFO substitution between Lstat and Open from
			// waiting for a writer; Fstat in the common path rejects that FD.
			fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				return nil, err
			}
			return os.NewFile(uintptr(fd), path), nil
		},
		sameFile: os.SameFile,
		lock: func(file contentionProbeFile) error {
			return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		},
		unlock: func(file contentionProbeFile) error {
			return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		},
		isContended: func(err error) bool {
			return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
		},
	}
}
