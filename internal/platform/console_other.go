//go:build !windows

package platform

import "errors"

// ConsoleProcessCount is only meaningful on Windows.
func ConsoleProcessCount() (int, error) {
	return 0, errors.New("console process list is a Windows concept")
}
