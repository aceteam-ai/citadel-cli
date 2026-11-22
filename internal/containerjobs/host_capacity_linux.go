//go:build linux

package containerjobs

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func hostFreeBytes(root *os.File) (uint64, error) {
	var fs unix.Statfs_t
	if unix.Fstatfs(int(root.Fd()), &fs) != nil || fs.Bsize <= 0 || fs.Bavail > math.MaxUint64/uint64(fs.Bsize) {
		return 0, ErrUnavailable
	}
	return fs.Bavail * uint64(fs.Bsize), nil
}

func (n *Native) hostCapacity(quota int64, limits HostLimits) error {
	opener := n.openHostRoot
	if opener == nil {
		opener = openContainerHostRoot
	}
	root, err := opener(n.root)
	if err != nil {
		return ErrUnavailable
	}
	defer root.Close()
	names, err := root.Readdirnames(maxHostEntries + 3)
	if err != nil && err != io.EOF || len(names) > maxHostEntries+2 {
		return ErrUnavailable
	}
	var volumes []string
	for _, name := range names {
		if name == "host.lock" || name == "exec-attempts" {
			continue
		}
		if !volumePattern.MatchString(name) {
			return ErrUnavailable
		}
		volumes = append(volumes, name)
	}
	if !limits.Valid() {
		return ErrUnavailable
	}
	if len(volumes) >= limits.MaxVolumes {
		return ErrBounds
	}
	var promised uint64
	for _, name := range volumes {
		bytes, err := unallocatedVolumePromise(root, name)
		if err != nil {
			return err
		}
		if bytes > math.MaxUint64-promised {
			return ErrUnavailable
		}
		promised += bytes
	}
	free := n.freeHostBytes
	if free == nil {
		free = hostFreeBytes
	}
	available, err := free(root)
	if err != nil {
		return ErrUnavailable
	}
	if promised > available {
		return ErrBounds
	}
	return limits.checkCapacity(quota, available-promised, len(volumes))
}

// Sparse images retain the parent's storage model. Charge their outstanding
// quota promises in addition to blocks already charged by filesystem free space.
func unallocatedVolumePromise(root *os.File, name string) (uint64, error) {
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, ErrUnavailable
	}
	dir := os.NewFile(uintptr(fd), "container quota reservation")
	defer dir.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 {
		return 0, ErrUnavailable
	}
	image, err := unix.Openat(fd, "volume.ext4", unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		// A fully removed volume leaves an empty, never-reused tombstone. Unknown
		// partial contents are not proof that its reservation can be released.
		entries, readErr := dir.Readdirnames(1)
		if len(entries) == 0 && readErr == io.EOF {
			return 0, nil
		}
		return 0, ErrUnavailable
	}
	if err != nil {
		return 0, ErrUnavailable
	}
	defer unix.Close(image)
	if unix.Fstat(image, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 || st.Nlink != 1 || !validQuota(st.Size) || st.Blocks < 0 || uint64(st.Blocks) > math.MaxUint64/512 {
		return 0, ErrUnavailable
	}
	allocated := uint64(st.Blocks) * 512
	quota := uint64(st.Size)
	if allocated >= quota {
		return 0, nil
	}
	return quota - allocated, nil
}

func (n *Native) syncVolumeReservation(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrUnavailable
	}
	file := os.NewFile(uintptr(fd), filepath.Base(dir))
	defer file.Close()
	if file.Sync() != nil {
		return ErrUnavailable
	}
	opener := n.openHostRoot
	if opener == nil {
		opener = openContainerHostRoot
	}
	root, err := opener(n.root)
	if err != nil {
		return ErrUnavailable
	}
	defer root.Close()
	if root.Sync() != nil {
		return ErrUnavailable
	}
	return nil
}

func (n *Native) currentPolicy() (HostPolicy, error) {
	if n.policy == nil {
		return HostPolicy{}, ErrUnavailable
	}
	p, err := n.policy()
	if err != nil {
		return HostPolicy{}, ErrUnavailable
	}
	if err = p.validate(); err != nil {
		return HostPolicy{}, err
	}
	return p, nil
}
