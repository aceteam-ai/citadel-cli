//go:build windows

package jobs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func validateHuddleOutboxDirectory(dir string, mustExist bool) error {
	info, err := os.Lstat(filepath.Clean(dir))
	if err != nil && os.IsNotExist(err) && !mustExist {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("huddle lifecycle outbox path is not a real directory")
	}
	return nil
}

func readHuddleOutboxFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, fmt.Errorf("invalid huddle lifecycle outbox record")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("oversized huddle lifecycle outbox record")
	}
	return raw, nil
}
