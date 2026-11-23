//go:build linux || darwin

package harnessseat

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func privateStoreSupported() bool { return true }

// Ancestors may be searchable/readable by others, but not writable by them.
// The final login directory still requires the stricter privateStoreInfo.
func ownedSeatParentInfo(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

func privateNoFollowFlags() int { return unix.O_NOFOLLOW | unix.O_NONBLOCK }

func privateStoreInfo(info os.FileInfo, dir bool) bool {
	if info == nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return false
	}
	if dir {
		return info.IsDir() && info.Mode() == os.ModeDir|0o700
	}
	return info.Mode() == 0o600 && info.Mode().IsRegular() && st.Nlink == 1
}

func privateStoreLock(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return ErrPrivateStore
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func privateStoreUnlock(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
