//go:build linux

package containerjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// ClaimExecution runs under Engine's host flock. The create-only record is
// synced before exec, survives process restart, and is never refunded or GC'd.
// A full or uncertain journal refuses further execution rather than forgetting.
func (n *Native) ClaimExecution(ctx context.Context, org, jobID string) error {
	if ctx.Err() != nil || !canonicalUUID(org) || jobID == "" || !validText(jobID, 256) {
		return ErrUnavailable
	}
	policy, err := n.currentPolicy()
	if err != nil || !policy.Limits.Valid() {
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
	if err := unix.Mkdirat(int(root.Fd()), "exec-attempts", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return ErrUnavailable
	}
	fd, err := unix.Openat(int(root.Fd()), "exec-attempts", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrUnavailable
	}
	dir := os.NewFile(uintptr(fd), "container exec attempts")
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return ErrUnavailable
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return ErrUnavailable
	}
	names, err := dir.Readdirnames(maxHostEntries + 1)
	if err != nil && err != io.EOF || len(names) >= maxHostEntries {
		return ErrUnavailable
	}
	for _, name := range names {
		if !hashPattern.MatchString(name) {
			return ErrUnavailable
		}
	}
	digest := sha256.Sum256([]byte("container-exec-v1\x00" + org + "\x00" + jobID))
	name := hex.EncodeToString(digest[:])
	fd, err = unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return ErrUnavailable
	}
	record := os.NewFile(uintptr(fd), "container exec attempt")
	_, writeErr := record.WriteString("attempt-v1\n")
	syncErr := n.syncExecJournal(record)
	closeErr := record.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || n.syncExecJournal(dir) != nil || n.syncExecJournal(root) != nil {
		return ErrUnavailable
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	return nil
}

func (n *Native) syncExecJournal(file *os.File) error {
	if n.syncJournal != nil {
		return n.syncJournal(file)
	}
	return file.Sync()
}
