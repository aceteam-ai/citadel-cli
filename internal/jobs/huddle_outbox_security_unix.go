//go:build !windows

package jobs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func validateHuddleOutboxDirectory(dir string, mustExist bool) error {
	clean := filepath.Clean(dir)
	info, err := os.Lstat(clean)
	if err != nil && os.IsNotExist(err) && !mustExist {
		clean = filepath.Dir(clean)
		for {
			info, err = os.Lstat(clean)
			if err == nil || !os.IsNotExist(err) {
				break
			}
			next := filepath.Dir(clean)
			if next == clean {
				break
			}
			clean = next
		}
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("huddle lifecycle outbox path is not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("huddle lifecycle outbox path is not owned by current uid")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("huddle lifecycle outbox path is group/other writable")
	}
	return nil
}

func readHuddleOutboxFile(path string, maxBytes int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, fmt.Errorf("invalid huddle lifecycle outbox record")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("oversized huddle lifecycle outbox record")
	}
	return raw, nil
}
