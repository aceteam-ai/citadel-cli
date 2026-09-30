//go:build linux

package cmd

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// waitForPapercraftProcessExit observes exit without reaping. Holding the
// zombie identity until the owner chooses cleanup prevents PID/PGID reuse from
// racing an explicit live shutdown.
func waitForPapercraftProcessExit(process *os.Process) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}
