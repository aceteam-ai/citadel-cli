// internal/jobs/disk_space_probe.go
//
// defaultAvailableDiskBytes production wiring for disk_space.go's #828
// preflight. Reuses gopsutil (already a direct dependency, and already used
// for the identical purpose in this SAME package -- see
// meetingDirUnderDiskPressure in meeting_retention.go) instead of hand-rolled
// per-platform syscalls, so this is one cross-platform file rather than a
// disk_space_unix.go (syscall.Statfs) / disk_space_windows.go
// (golang.org/x/sys/windows.GetDiskFreeSpaceEx) pair.
//
// On Unix, gopsutil's disk.Usage(path).Free is computed as
// Bavail*Bsize (verified against gopsutil's own disk_unix.go) -- i.e. space
// available to an UNPRIVILEGED user (excluding filesystem-reserved blocks),
// the same semantic the preflight needs, not the larger Bfree total.
package jobs

import (
	"fmt"

	"github.com/shirou/gopsutil/v3/disk"
)

// defaultAvailableDiskBytes returns free space available at path's
// filesystem/volume. Overridable via availableDiskBytesFn (disk_space.go) for
// tests.
func defaultAvailableDiskBytes(path string) (uint64, error) {
	u, err := disk.Usage(path)
	if err != nil {
		return 0, err
	}
	if u == nil {
		return 0, fmt.Errorf("disk.Usage(%s) returned no usage data", path)
	}
	return u.Free, nil
}
