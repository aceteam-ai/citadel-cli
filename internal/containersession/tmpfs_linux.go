//go:build linux

package containersession

import "golang.org/x/sys/unix"

const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// IsTmpfs reports whether path is on a memory-backed filesystem, so a
// materialized credential cannot reach a disk. It is the production answer to
// Runner.IsTmpfs.
func IsTmpfs(path string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false, err
	}
	return int64(st.Type) == tmpfsMagic || int64(st.Type) == ramfsMagic, nil
}
