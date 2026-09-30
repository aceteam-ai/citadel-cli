//go:build !windows

package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// papercraftProcessTree keeps live cancellation separate from post-Wait
// cleanup. os/exec invokes cancelLive before it can reap a CommandContext
// child, so the numeric PID/PGID still identifies that child. After Wait, only
// the random systemd scope remains a safe identity authority.
type papercraftProcessTree struct {
	cmd       *exec.Cmd
	stopScope func() error

	stateMu     sync.Mutex
	waitStarted bool
	groupOnce   sync.Once
	groupErr    error
	scopeOnce   sync.Once
	scopeErr    error
}

func configurePapercraftProcessTree(limited *renderCommand) *papercraftProcessTree {
	tree := &papercraftProcessTree{cmd: limited.cmd, stopScope: limited.stopSystemdScope}
	tree.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tree.cmd.Cancel = tree.cancelForContext
	return tree
}

// terminateLiveBeforeWait is for explicit shutdown paths that own the
// unreaped child. beginWait serializes the handoff so no numeric PGID signal
// can occur once reaping may start.
func (t *papercraftProcessTree) terminateLiveBeforeWait() error {
	if t.cmd.Process == nil {
		return nil
	}
	t.stateMu.Lock()
	if !t.waitStarted {
		t.killLiveGroup()
		t.waitStarted = true
	} else if err := t.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.groupErr = errors.Join(t.groupErr, fmt.Errorf("kill render root process: %w", err))
	}
	t.stateMu.Unlock()
	return errors.Join(t.processErr(), t.stopSystemdScope())
}

// cancelForContext may run concurrently with Wait. Before beginWait, the root
// is intentionally unreaped and the PGID is identity-safe. Afterwards, use
// os.Process.Kill, whose pidfd/process handle cannot target a reused PID, and
// rely on the unique systemd scope for descendants.
func (t *papercraftProcessTree) cancelForContext() error {
	if t.cmd.Process == nil {
		return nil
	}
	t.stateMu.Lock()
	if !t.waitStarted {
		t.killLiveGroup()
	} else if err := t.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.groupErr = errors.Join(t.groupErr, fmt.Errorf("kill render root process: %w", err))
	}
	t.stateMu.Unlock()
	return errors.Join(t.processErr(), t.stopSystemdScope())
}

func (t *papercraftProcessTree) beginWait() {
	t.stateMu.Lock()
	t.waitStarted = true
	t.stateMu.Unlock()
}

func (t *papercraftProcessTree) killLiveGroup() {
	t.groupOnce.Do(func() {
		if err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.groupErr = fmt.Errorf("kill live render process group: %w", err)
		}
	})
}

func (t *papercraftProcessTree) processErr() error {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.groupErr
}

// cleanupAfterWait never signals a numeric process identity: the root may
// already have been reaped and its PID/PGID reused by an unrelated process.
func (t *papercraftProcessTree) cleanupAfterWait() error {
	if t.cmd.Process == nil {
		return nil
	}
	return errors.Join(t.processErr(), t.stopSystemdScope())
}

func (t *papercraftProcessTree) stopSystemdScope() error {
	t.scopeOnce.Do(func() {
		if t.stopScope != nil {
			t.scopeErr = t.stopScope()
		}
	})
	return t.scopeErr
}
