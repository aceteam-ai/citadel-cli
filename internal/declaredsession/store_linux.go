//go:build linux

package declaredsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func OpenStore(nodeDir string) (*Store, error) {
	if !filepath.IsAbs(nodeDir) || os.Geteuid() == 0 {
		return nil, ErrUnsafe
	}
	// The selected root and its ancestors are a trusted configuration boundary;
	// aliases there are permitted. All descendants use anchored no-follow opens.
	fd, err := unix.Open(nodeDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(fd), nodeDir)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) || st.Mode&0o022 != 0 {
		_ = file.Close()
		return nil, ErrUnsafe
	}
	return &Store{root: filepath.Clean(nodeDir), anchor: file, uid: st.Uid}, nil
}

func (s *Store) OpenSession(name string, create bool) (*SessionStore, error) {
	if ValidateName(name) != nil {
		return nil, ErrConfig
	}
	parent, err := openDirectoryAt(s.anchor, "sessions", create, s.uid)
	if err != nil {
		return nil, err
	}
	dir, err := openDirectoryAt(parent, name, create, s.uid)
	parentCloseErr := parent.Close()
	if err != nil {
		return nil, err
	}
	if parentCloseErr != nil {
		_ = dir.Close()
		return nil, ErrUnavailable
	}
	path := filepath.Join(s.root, "sessions", name)
	return &SessionStore{store: s, dir: dir, paths: Paths{
		Name: name, Dir: path, Socket: filepath.Join(path, "host.sock"),
		State: filepath.Join(path, "state.json"), OperationLock: filepath.Join(path, "operation.lock"), OwnerLock: filepath.Join(path, "host.lock"),
	}}, nil
}

func openDirectoryAt(parent *os.File, name string, create bool, uid uint32) (*os.File, error) {
	return openDirectoryAtWith(parent, name, create, uid, func(file *os.File) error { return file.Sync() })
}

func openDirectoryAtWith(parent *os.File, name string, create bool, uid uint32, syncFile func(*os.File) error) (*os.File, error) {
	created := false
	if create {
		err := unix.Mkdirat(int(parent.Fd()), name, 0o700)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, ErrUnavailable
		}
		created = err == nil
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, ErrUnsafe
	}
	file := os.NewFile(uintptr(fd), name)
	if created && unix.Fchmod(fd, 0o700) != nil {
		_ = file.Close()
		return nil, ErrUnavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uid || st.Mode&0o7777 != 0o700 {
		_ = file.Close()
		return nil, ErrUnsafe
	}
	// Explicit creation also re-syncs an existing directory: an earlier attempt
	// may have left its new name visible after an uncertain durability failure.
	// Read-only opens never sync or create anything.
	if create && (syncFile(file) != nil || syncFile(parent) != nil) {
		_ = file.Close()
		return nil, ErrUnavailable
	}
	return file, nil
}

func (s *SessionStore) SocketAnchor() (*os.File, error) {
	return openDirectoryAt(s.dir, ".", false, s.store.uid)
}

func validFile(fd int, uid uint32) (unix.Stat_t, error) {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || !privateFileStat(st, uid) {
		return st, ErrUnsafe
	}
	return st, nil
}

func privateFileStat(st unix.Stat_t, uid uint32) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Uid == uid && st.Mode&0o7777 == 0o600 && st.Nlink == 1
}

func readPrivateAt(parent *os.File, name string, max int, uid uint32) ([]byte, bool, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, ErrUnsafe
	}
	file := os.NewFile(uintptr(fd), name)
	before, validErr := validFile(fd, uid)
	if validErr != nil || before.Size < 0 || before.Size > int64(max) {
		_ = file.Close()
		return nil, false, ErrUnsafe
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, int64(max)+1))
	after, statErr := validFile(fd, uid)
	var pathStat unix.Stat_t
	pathErr := unix.Fstatat(int(parent.Fd()), name, &pathStat, unix.AT_SYMLINK_NOFOLLOW)
	closeErr := file.Close()
	if readErr != nil || statErr != nil || pathErr != nil || closeErr != nil || len(raw) > max || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || pathStat.Dev != after.Dev || pathStat.Ino != after.Ino {
		return nil, false, ErrUnavailable
	}
	return raw, true, nil
}

func readPrivatePath(path string, max int, uid uint32) ([]byte, error) {
	fd, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	parent := os.NewFile(uintptr(fd), "env parent")
	raw, present, readErr := readPrivateAt(parent, filepath.Base(path), max, uid)
	closeErr := parent.Close()
	if readErr != nil {
		return nil, readErr
	}
	if !present || closeErr != nil {
		return nil, ErrUnavailable
	}
	return raw, nil
}

func openMutableAt(parent *os.File, name string, flags int, uid uint32) (*os.File, error) {
	return openMutableAtWith(parent, name, flags, uid, func(file *os.File) error { return file.Sync() })
}

func openMutableAtWith(parent *os.File, name string, flags int, uid uint32, syncFile func(*os.File) error) (*os.File, error) {
	policy := unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	fd, err := unix.Openat(int(parent.Fd()), name, flags|policy|unix.O_CREAT|unix.O_EXCL, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(int(parent.Fd()), name, flags|policy, 0)
	}
	if err != nil {
		return nil, ErrUnsafe
	}
	file := os.NewFile(uintptr(fd), name)
	if created && unix.Fchmod(fd, 0o600) != nil {
		_ = file.Close()
		return nil, ErrUnavailable
	}
	st, validErr := validFile(fd, uid)
	var pathStat unix.Stat_t
	if validErr != nil || unix.Fstatat(int(parent.Fd()), name, &pathStat, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Dev != pathStat.Dev || st.Ino != pathStat.Ino {
		_ = file.Close()
		return nil, ErrUnsafe
	}
	if created && syncFile(file) != nil {
		_ = file.Close()
		return nil, ErrUnavailable
	}
	// Always sync the parent, including an existing-file retry after an earlier
	// failed publication sync. Never repair or unlink that existing inode.
	if syncFile(parent) != nil {
		_ = file.Close()
		return nil, ErrUnavailable
	}
	return file, nil
}

func (s *SessionStore) acquire(ctx context.Context, name string) (*fileGuard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openMutableAt(s.dir, name, unix.O_RDWR, s.store.uid)
	if err != nil {
		return nil, err
	}
	err = lockPrivateRecord(ctx, name, func() error { return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) })
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
		if name == "record.lock" {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &fileGuard{file: file, session: s, kind: name}, nil
}

func lockPrivateRecord(ctx context.Context, name string, tryLock func() error) error {
	for {
		if ctx.Err() != nil {
			return ErrBusy
		}
		err := tryLock()
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			return ErrUnavailable
		}
		// Operator/owner guards still reject concurrent ownership immediately.
		// Short record writers instead wait for normal fsync/rename contention,
		// with the caller's finite deadline. Exhaustion is busy, not corruption.
		if name != "record.lock" {
			return ErrBusy
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ErrBusy
		}
	}
}

func (g *fileGuard) close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	unlockErr := unix.Flock(int(g.file.Fd()), unix.LOCK_UN)
	closeErr := g.file.Close()
	if unlockErr != nil || closeErr != nil {
		return ErrUnavailable
	}
	return nil
}

func writePrivateAt(parent *os.File, name string, raw []byte, max int, uid uint32) error {
	return writePrivateAtWith(parent, name, raw, max, uid, recordIO{
		write: func(file *os.File, body []byte) (int, error) { return file.Write(body) },
		sync:  func(file *os.File) error { return file.Sync() },
		close: func(file *os.File) error { return file.Close() },
	})
}

// Private per-call dependencies exercise actual publication failure boundaries;
// production always supplies descriptor operations, never global mutable hooks.
type recordIO struct {
	write func(*os.File, []byte) (int, error)
	sync  func(*os.File) error
	close func(*os.File) error
}

func writePrivateAtWith(parent *os.File, name string, raw []byte, max int, uid uint32, ops recordIO) (retErr error) {
	if len(raw) > max {
		return ErrUnsafe
	}
	// Refuse supplied unsafe destinations; never repair their mode or follow a
	// final symlink. Record writers are serialized under the separate record lock.
	if _, _, err := readPrivateAt(parent, name, max, uid); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrUnavailable
	}
	tempName := ".record-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(parent.Fd()), tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return ErrUnavailable
	}
	file := os.NewFile(uintptr(fd), tempName)
	// Same-UID private parent is the stated mutation boundary; no untrusted
	// writer can substitute a staging name under this contract.
	defer func() {
		if err := unix.Unlinkat(int(parent.Fd()), tempName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			retErr = ErrUnavailable
		}
	}()
	if unix.Fchmod(fd, 0o600) != nil {
		_ = file.Close()
		return ErrUnavailable
	}
	if _, err := validFile(fd, uid); err != nil {
		_ = file.Close()
		return err
	}
	n, writeErr := ops.write(file, raw)
	syncErr := ops.sync(file)
	closeErr := ops.close(file)
	if writeErr != nil || n != len(raw) || syncErr != nil || closeErr != nil {
		return ErrUnavailable
	}
	if unix.Renameat(int(parent.Fd()), tempName, int(parent.Fd()), name) != nil {
		return ErrUnavailable
	}
	if ops.sync(parent) != nil {
		// Rename may already be visible. No rollback or durable-success claim.
		return ErrUnavailable
	}
	return nil
}

func removePrivateAt(parent *os.File, name string, uid uint32) error {
	if _, present, err := readPrivateAt(parent, name, MaxStateBytes, uid); err != nil || !present {
		if err != nil {
			return err
		}
		return ErrConflict
	}
	if unix.Unlinkat(int(parent.Fd()), name, 0) != nil || parent.Sync() != nil {
		return ErrUnavailable
	}
	return nil
}

func closePrivate(file *os.File) error {
	if file == nil || file.Close() != nil {
		return ErrUnavailable
	}
	return nil
}

func appendLogAt(parent *os.File, name string, raw []byte, policy LogPolicy, uid uint32) error {
	for len(raw) > 0 {
		file, err := openMutableAt(parent, name, unix.O_WRONLY|unix.O_APPEND, uid)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil || info.Size() > policy.RotateBytes {
			_ = file.Close()
			return ErrUnsafe
		}
		if info.Size() == policy.RotateBytes {
			if file.Close() != nil {
				return ErrUnavailable
			}
			if err := rotateLogAt(parent, name, policy.Keep, uid); err != nil {
				return err
			}
			continue
		}
		chunk := len(raw)
		if remaining := policy.RotateBytes - info.Size(); int64(chunk) > remaining {
			chunk = int(remaining)
		}
		n, writeErr := file.Write(raw[:chunk])
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || n != chunk || syncErr != nil || closeErr != nil {
			return ErrUnavailable
		}
		raw = raw[chunk:]
	}
	return nil
}

func inspectLogAt(parent *os.File, name string, uid uint32) (bool, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, ErrUnsafe
	}
	file := os.NewFile(uintptr(fd), name)
	_, validErr := validFile(fd, uid)
	closeErr := file.Close()
	if validErr != nil || closeErr != nil {
		return false, ErrUnsafe
	}
	return true, nil
}

func rotateLogAt(parent *os.File, name string, keep int, uid uint32) error {
	for i := 0; i <= keep; i++ {
		candidate := name
		if i > 0 {
			candidate += "." + strconv.Itoa(i)
		}
		if _, err := inspectLogAt(parent, candidate, uid); err != nil {
			return err
		}
	}
	oldest := name + "." + strconv.Itoa(keep)
	if present, err := inspectLogAt(parent, oldest, uid); err != nil {
		return err
	} else if present && unix.Unlinkat(int(parent.Fd()), oldest, 0) != nil {
		return ErrUnavailable
	}
	for i := keep - 1; i >= 0; i-- {
		from := name
		if i > 0 {
			from += "." + strconv.Itoa(i)
		}
		present, err := inspectLogAt(parent, from, uid)
		if err != nil {
			return err
		}
		if present && unix.Renameat(int(parent.Fd()), from, int(parent.Fd()), name+"."+strconv.Itoa(i+1)) != nil {
			return ErrUnavailable
		}
	}
	if parent.Sync() != nil {
		return ErrUnavailable
	}
	return nil
}

func readLogTailAt(parent *os.File, name string, max int, uid uint32) ([]byte, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrUnsafe
	}
	file := os.NewFile(uintptr(fd), name)
	before, validErr := validFile(fd, uid)
	if validErr != nil || before.Size < 0 || before.Size > 64<<20 {
		_ = file.Close()
		return nil, ErrUnsafe
	}
	start := before.Size - int64(max)
	if start < 0 {
		start = 0
	}
	raw, readErr := io.ReadAll(io.NewSectionReader(file, start, before.Size-start))
	after, statErr := validFile(fd, uid)
	closeErr := file.Close()
	// Append-only growth is allowed: the section pins the observed end offset.
	if readErr != nil || statErr != nil || closeErr != nil || after.Size < before.Size || after.Dev != before.Dev || after.Ino != before.Ino {
		return nil, ErrUnavailable
	}
	return raw, nil
}
