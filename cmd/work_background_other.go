//go:build !windows

package cmd

import (
	"errors"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/service"
)

// errBackgroundWindowsOnly: handleBackgroundModes refuses before reaching these.
var errBackgroundWindowsOnly = errors.New("the per-user background worker is Windows-only")

func startDetachedSupervisor(exe string, args []string, logFile *os.File) (int, error) {
	return 0, errBackgroundWindowsOnly
}

func startSupervisedWorker(exe string, args []string) (service.SupervisedChild, error) {
	return nil, errBackgroundWindowsOnly
}
