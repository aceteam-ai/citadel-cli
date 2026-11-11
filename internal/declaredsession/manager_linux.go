//go:build linux

package declaredsession

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
	"golang.org/x/sys/unix"
)

func nativeManagerOps() managerOps {
	return managerOps{
		inspect:      procscope.Inspect,
		process:      procscope.ObserveProcess,
		stop:         procscope.StopExact,
		dial:         func(ctx context.Context, session *SessionStore) (managerPeer, error) { return DialHolder(ctx, session) },
		attachDial:   DialHolder,
		socket:       managerSocketIdentity,
		removeSocket: removeManagerSocket,
		launch:       launchManagedHolder,
	}
}

func managerSocketIdentity(session *SessionStore) (managerSocket, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(session.DirectoryFD(), holderSocketName, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == unix.ENOENT {
		return managerSocket{}, nil
	}
	if err != nil || stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Uid != uint32(os.Getuid()) || stat.Mode&0o777 != 0o600 || stat.Nlink != 1 {
		return managerSocket{}, ErrManager
	}
	return managerSocket{Present: true, Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

// Called only under the operation AND owner guards after exact scope/birth
// absence. A replacement pathname is never unlinked, even if it is a socket.
func removeManagerSocket(session *SessionStore, expected managerSocket) error {
	observed, err := managerSocketIdentity(session)
	if err != nil {
		return err
	}
	if !observed.Present {
		return nil // Graceful holder cleanup already removed its captured inode.
	}
	if !expected.Present || observed != expected || unix.Unlinkat(session.DirectoryFD(), holderSocketName, 0) != nil {
		return ErrManager
	}
	return session.dir.Sync()
}

func launchManagedHolder(ctx context.Context, executable, nodeDir, name, incarnation, version string, restart bool) (int, error) {
	if ctx.Err() != nil || os.Getuid() <= 0 || os.Getuid() != os.Geteuid() || ValidateName(name) != nil || !validIncarnation(incarnation) || !filepath.IsAbs(executable) || !filepath.IsAbs(nodeDir) {
		return 0, ErrManager
	}
	unit, _ := ScopeName(name)
	origin, err := procscope.CurrentControlGroup()
	if err != nil {
		return 0, ErrManager
	}
	spec, err := procscope.NewUserScope(unit, uint32(os.Getuid()), origin)
	if err != nil {
		return 0, ErrManager
	}
	// This is an isolation boundary, not a PATH-based command lookup. The
	// fixed system executable must be root-owned, executable and not writable
	// by group/other. Caller-controlled PATH entries can never provide it.
	const systemdRun = "/usr/bin/systemd-run"
	var systemdStat unix.Stat_t
	if unix.Stat(systemdRun, &systemdStat) != nil || !trustedSystemdRun(systemdStat.Mode, systemdStat.Uid) {
		return 0, ErrManager
	}
	args := []string{"session-host", name, "--node-config-dir=" + nodeDir, "--expected-unit=" + unit, "--launch-id=" + incarnation, "--origin-cgroup=" + origin}
	if restart {
		args = append(args, "--restart-command")
	}
	plan, err := spec.Launch(systemdRun, executable, args)
	if err != nil {
		return 0, ErrManager
	}
	uid := strconv.Itoa(os.Getuid())
	owner, err := user.LookupId(uid)
	if err != nil || owner.Uid != uid || !filepath.IsAbs(owner.HomeDir) {
		return 0, ErrManager
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, ErrManager
	}
	defer null.Close()
	// No inherited device token, auth environment, terminal, pipe or worker
	// context. The holder loads its declared harness environment privately.
	cmd := exec.Command(plan.Binary, plan.Args...)
	cmd.Env = []string{"HOME=" + owner.HomeDir, "USER=" + owner.Username, "LOGNAME=" + owner.Username, "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "XDG_RUNTIME_DIR=/run/user/" + uid, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	cmd.Dir = nodeDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, ErrManager
	}
	pid := cmd.Process.Pid
	// systemd-run --scope execs the holder in this PID. Wait reaps only when
	// that durable process actually exits; client cancellation never kills it.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

func trustedSystemdRun(mode, uid uint32) bool {
	return uid == 0 && mode&unix.S_IFMT == unix.S_IFREG && mode&0o022 == 0 && mode&0o111 != 0
}
