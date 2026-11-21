//go:build windows

package localcobrowse

import (
	"errors"

	"golang.org/x/sys/windows"
)

func uploadRenameRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
