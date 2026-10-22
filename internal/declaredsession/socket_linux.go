//go:build linux

package declaredsession

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
	"golang.org/x/sys/unix"
)

const holderSocketName = "host.sock"

type holderListener struct {
	listener *net.UnixListener
	anchor   *os.File
	device   uint64
	inode    uint64
}

func socketAddress(anchor *os.File) string {
	return "/proc/self/fd/" + strconv.Itoa(int(anchor.Fd())) + "/" + holderSocketName
}

func listenHolder(session *SessionStore) (*holderListener, error) {
	anchor, err := session.SocketAnchor()
	if err != nil {
		return nil, ErrUnsafe
	}
	closeAnchor := true
	defer func() {
		if closeAnchor {
			_ = anchor.Close()
		}
	}()
	var stat unix.Stat_t
	// Never unlink a stale socket here: only the manager may classify a lost
	// holder. An unexpected existing path is a refusal, not recovery authority.
	if err := unix.Fstatat(int(anchor.Fd()), holderSocketName, &stat, unix.AT_SYMLINK_NOFOLLOW); err != unix.ENOENT {
		return nil, ErrUnsafe
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketAddress(anchor), Net: "unix"})
	if err != nil {
		return nil, ErrUnavailable
	}
	listener.SetUnlinkOnClose(false)
	// Pin the actual bound socket inode before chmod. /proc/self/fd on O_PATH
	// addresses that inode, not a newly substituted name. The private owner is
	// trusted not to race directory renames; root is outside this boundary.
	fd, err := unix.Openat(int(anchor.Fd()), holderSocketName, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = listener.Close()
		return nil, ErrUnsafe
	}
	defer unix.Close(fd)
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		_ = listener.Close()
		return nil, ErrUnsafe
	}
	if unix.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), 0600) != nil {
		_ = listener.Close()
		return nil, ErrUnsafe
	}
	var after unix.Stat_t
	if unix.Fstatat(int(anchor.Fd()), holderSocketName, &after, unix.AT_SYMLINK_NOFOLLOW) != nil || after.Dev != stat.Dev || after.Ino != stat.Ino || after.Mode&0777 != 0600 {
		_ = listener.Close()
		return nil, ErrUnsafe
	}
	closeAnchor = false
	return &holderListener{listener: listener, anchor: anchor, device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func (l *holderListener) Accept() (net.Conn, error) { return l.listener.Accept() }

func (l *holderListener) stopAccept() { _ = l.listener.Close() }

func (l *holderListener) Close() error {
	_ = l.listener.Close() // the accept loop may already have been joined
	var stat unix.Stat_t
	err := unix.Fstatat(int(l.anchor.Fd()), holderSocketName, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil && uint64(stat.Dev) == l.device && stat.Ino == l.inode && stat.Mode&unix.S_IFMT == unix.S_IFSOCK {
		err = unix.Unlinkat(int(l.anchor.Fd()), holderSocketName, 0)
		if err == nil {
			err = l.anchor.Sync()
		}
	} else {
		err = ErrUnsafe // never remove a replacement, including a new socket
	}
	anchorErr := l.anchor.Close()
	if err != nil || anchorErr != nil {
		return ErrUnavailable
	}
	return nil
}

func dialHolderSocket(ctx context.Context, session *SessionStore) (net.Conn, int, error) {
	anchor, err := session.SocketAnchor()
	if err != nil {
		return nil, 0, ErrUnsafe
	}
	defer anchor.Close()
	var stat unix.Stat_t
	if unix.Fstatat(int(anchor.Fd()), holderSocketName, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&0777 != 0600 || stat.Uid != uint32(os.Getuid()) {
		return nil, 0, ErrPeer
	}
	dialCtx, cancel := context.WithTimeout(ctx, operationWait)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "unix", socketAddress(anchor))
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	pid, err := sameUIDPeer(conn)
	if err != nil {
		_ = conn.Close()
		return nil, 0, err
	}
	return conn, pid, nil
}

func sameUIDPeer(conn net.Conn) (int, error) {
	socket, ok := conn.(*net.UnixConn)
	if !ok || os.Getuid() == 0 || os.Getuid() != os.Geteuid() {
		return 0, ErrPeer
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, ErrPeer
	}
	var credential *unix.Ucred
	var inspectErr error
	if raw.Control(func(fd uintptr) {
		credential, inspectErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}) != nil || inspectErr != nil || credential == nil || credential.Uid != uint32(os.Getuid()) || credential.Pid <= 0 {
		return 0, ErrPeer
	}
	return int(credential.Pid), nil
}

func observeHolderPeer(pid int) (ProcessIdentity, error) {
	process, err := procscope.ObserveProcess(pid)
	if err != nil || process.UID != uint32(os.Getuid()) {
		return ProcessIdentity{}, ErrPeer
	}
	return fromScopeProcess(process), nil
}
