//go:build linux

package declaredsession

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/procscope"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type nativeChild struct {
	cmd       *exec.Cmd
	master    *os.File
	fd        int // captured once; no File.Fd calls after NONBLOCK is set
	identity  ProcessIdentity
	exited    chan struct{}
	observeOK bool
	once      sync.Once
	exit      Exit
	reapErr   error
}

func spawnTerminal(declaration Declaration, argv, env []string, generation uint64) (holderChild, error) {
	if len(argv) == 0 || !filepathAbsolute(argv[0]) {
		return nil, ErrConfig
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = declaration.Cwd, append([]string(nil), env...)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		return nil, errChildIO
	}
	child := &nativeChild{cmd: cmd, master: master, fd: int(master.Fd()), exited: make(chan struct{})}
	identity, err := procscope.ObserveProcess(cmd.Process.Pid)
	if err == nil {
		child.identity = fromScopeProcess(identity)
	}
	// A partially started child is still returned for the caller's mandatory
	// cleanup/quarantine path. It must never be lost on setup failure.
	go child.observe()
	if err != nil || child.identity.PID != cmd.Process.Pid || unix.SetNonblock(child.fd, true) != nil {
		return child, errChildIO
	}
	return child, nil
}

func (c *nativeChild) Identity() ProcessIdentity { return c.identity }
func (c *nativeChild) Exited() <-chan struct{}   { return c.exited }

func (c *nativeChild) observe() {
	defer close(c.exited)
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, c.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		c.observeOK = err == nil
		return // WNOWAIT retains the leader and fences numeric PID/PGID reuse
	}
}

func (c *nativeChild) Read(ctx context.Context, buffer []byte) (int, error) {
	for ctx.Err() == nil {
		n, err := unix.Read(c.fd, buffer)
		if n > 0 {
			return n, nil
		}
		if err == unix.EINTR {
			continue
		}
		if err == unix.EAGAIN {
			if pollTerminal(ctx, c.fd, unix.POLLIN) != nil {
				return 0, errChildIO
			}
			continue
		}
		if err == nil || err == unix.EIO {
			return 0, io.EOF
		}
		return 0, errChildIO
	}
	return 0, errChildIO
}

func (c *nativeChild) Write(ctx context.Context, data []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, errChildIO
	}
	n, err := unix.Write(c.fd, data)
	if err == unix.EAGAIN || err == unix.EINTR {
		if pollTerminal(ctx, c.fd, unix.POLLOUT) == nil {
			// Return to the serial worker to recheck controller epoch before
			// retrying. No blocked os.File.Write can outlive a transfer barrier.
			return 0, nil
		}
	}
	if err != nil {
		return 0, errChildIO
	}
	return n, nil
}

func pollTerminal(ctx context.Context, fd int, events int16) error {
	if ctx.Err() != nil {
		return errChildIO
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
	_, err := unix.Poll(fds, 50)
	if err != nil && err != unix.EINTR {
		return errChildIO
	}
	return nil
}

func (c *nativeChild) Resize(cols, rows uint16) error {
	if !validDimensions(cols, rows) || unix.IoctlSetWinsize(c.fd, unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows}) != nil {
		return errChildIO
	}
	return nil
}

func (c *nativeChild) Repaint() error { return c.SignalGroup(int(unix.SIGWINCH)) }

func (c *nativeChild) SignalGroup(signal int) error {
	current, err := procscope.ObserveProcess(c.cmd.Process.Pid)
	if err != nil || fromScopeProcess(current) != c.identity || c.identity.PID <= 0 || unix.Getpgrp() == c.identity.PID {
		return errChildIO
	}
	// The exact child remains unreaped, even after exit. Never signal a numeric
	// group after Reap, and never treat a state-file PID as this capability.
	if err := unix.Kill(-c.identity.PID, syscall.Signal(signal)); err != nil && err != unix.ESRCH {
		return errChildIO
	}
	return nil
}

func (c *nativeChild) Reap() (Exit, error) {
	select {
	case <-c.exited:
	default:
		return Exit{}, errChildIO
	}
	if !c.observeOK {
		return Exit{}, errChildIO
	}
	c.once.Do(func() {
		err := c.cmd.Wait()
		status := c.cmd.ProcessState
		if status == nil {
			c.reapErr = errChildIO
			return
		}
		c.exit = Exit{Code: status.ExitCode(), ObservedAt: time.Now().UTC()}
		if wait, ok := status.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
			c.exit.Signal = int(wait.Signal())
		}
		if err != nil {
			if _, expected := err.(*exec.ExitError); !expected {
				c.reapErr = errChildIO
			}
		}
	})
	return c.exit, c.reapErr
}

func (c *nativeChild) Close() error { return c.master.Close() }

func nativeOwner() (ProcessIdentity, error) {
	if os.Getuid() == 0 || os.Getuid() != os.Geteuid() {
		return ProcessIdentity{}, ErrPeer
	}
	identity, err := procscope.ObserveProcess(os.Getpid())
	if err != nil {
		return ProcessIdentity{}, ErrPeer
	}
	return fromScopeProcess(identity), nil
}
