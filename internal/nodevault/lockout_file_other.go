//go:build !linux && !darwin && !windows

package nodevault

import (
	"os"
	"time"
)

func openLockoutFile(string) (*os.File, error) {
	return nil, ErrLockoutUnavailable
}

func lockLockoutFile(*os.File, time.Time) error {
	return ErrLockoutUnavailable
}

func unlockLockoutFile(*os.File) {}
