//go:build !linux && !darwin && !windows

package nodevault

import (
	"os"
	"time"
)

const lockoutStatusSupported = false

func openExistingLockoutFile(string) (*os.File, error) {
	return nil, ErrLockoutUnavailable
}

func openLockoutFile(string) (*os.File, error) {
	return nil, ErrLockoutUnavailable
}

func lockLockoutFile(*os.File, time.Time) error {
	return ErrLockoutUnavailable
}

func unlockLockoutFile(*os.File) {}
