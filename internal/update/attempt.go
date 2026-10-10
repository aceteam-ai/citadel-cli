package update

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// UpdateAttempt owns all files created for one download/extraction attempt.
// Its directory is private and its cleanup never follows a replaced root.
type UpdateAttempt struct {
	Dir       string
	Candidate string

	identity os.FileInfo
	once     sync.Once
	err      error
}

func NewUpdateAttempt() (*UpdateAttempt, error) {
	if err := EnsureUpdateDir(); err != nil {
		return nil, fmt.Errorf("create update parent: %w", err)
	}
	dir, identity, err := createPrivateAttemptDir(GetUpdateDir())
	if err != nil {
		return nil, fmt.Errorf("create private update attempt: %w", err)
	}
	return &UpdateAttempt{
		Dir:       dir,
		Candidate: filepath.Join(dir, candidateFileName()),
		identity:  identity,
	}, nil
}

func candidateFileName() string {
	if runtime.GOOS == "windows" {
		return "citadel.exe"
	}
	return "citadel"
}

// Cleanup removes only this attempt's original directory. A changed identity
// is refused, and repeated calls return the first result.
func (a *UpdateAttempt) Cleanup() error {
	if a == nil {
		return nil
	}
	a.once.Do(func() {
		if a.Dir == "" || a.identity == nil {
			return
		}
		current, err := os.Lstat(a.Dir)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			a.err = err
			return
		}
		if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(a.identity, current) {
			a.err = fmt.Errorf("refusing cleanup of replaced update attempt directory")
			return
		}
		a.err = os.RemoveAll(a.Dir)
	})
	return a.err
}
