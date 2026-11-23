//go:build windows

package platform

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

// StartInKillOnCloseJob starts cmd suspended inside a new kill-on-close Job
// Object and resumes it (startInKillOnCloseJob). The caller owns the returned
// handle: TerminateJobObject ends the whole process tree, and closing the last
// handle, including when the caller exits, kills every process in the job. The
// per-user worker supervisor uses it so a worker can never outlive it.
func StartInKillOnCloseJob(cmd *exec.Cmd) (windows.Handle, error) {
	return startInKillOnCloseJob(cmd)
}
