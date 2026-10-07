//go:build windows

package memory

import (
	"fmt"
	"os"
)

func openRegularTranscript(path string) (*os.File, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("transcript is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	openedInfo, statErr := f.Stat()
	currentInfo, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !openedInfo.Mode().IsRegular() ||
		currentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, currentInfo) {
		_ = f.Close()
		return nil, fmt.Errorf("transcript is not a stable regular file")
	}
	return f, nil
}
