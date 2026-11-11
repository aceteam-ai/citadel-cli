//go:build !windows

package hpc

import (
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ExecProcess is a real child. With OwnGroup it leads its own process group
// and every signal goes to the whole group, so a model server's helper
// processes die with it.
type ExecProcess struct {
	cmd      *exec.Cmd
	ownGroup bool
	done     chan error
}

// StartProcess starts argv with env (the full environment) and output to w.
func StartProcess(argv, env []string, w io.Writer, ownGroup bool) (*ExecProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout = w
	cmd.Stderr = w
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &ExecProcess{cmd: cmd, ownGroup: ownGroup, done: make(chan error, 1)}
	go func() {
		p.done <- cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Pid returns the child's process id.
func (p *ExecProcess) Pid() int { return p.cmd.Process.Pid }

// Done receives the exit error once.
func (p *ExecProcess) Done() <-chan error { return p.done }

// Signal delivers sig to the child, or to its whole group.
func (p *ExecProcess) Signal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return p.cmd.Process.Signal(sig)
	}
	if p.ownGroup {
		return syscall.Kill(-p.cmd.Process.Pid, s)
	}
	return p.cmd.Process.Signal(s)
}

// StopGroup sends SIGTERM, waits up to grace, then SIGKILLs the group. When
// the group leader has already exited it still kills any member left.
func StopGroup(p Process, grace time.Duration) {
	_ = p.Signal(syscall.SIGTERM)
	select {
	case <-p.Done():
	case <-time.After(grace):
		_ = p.Signal(syscall.SIGKILL)
		<-p.Done()
	}
	// A helper that ignored SIGTERM outlives its leader: sweep the group.
	_ = p.Signal(syscall.SIGKILL)
}
