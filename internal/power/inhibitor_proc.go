//go:build darwin || linux

package power

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

// procInhibitor holds a sleep-inhibition assertion as a long-lived child
// process (macOS `caffeinate -s`, Linux `systemd-inhibit ... sleep infinity`).
// Killing the process releases the assertion, and the OS reaps the child if
// Citadel itself dies, so the machine can never be left permanently awake.
//
// It is shared by the darwin and linux builds; each supplies the command +
// args via newProcInhibitor.
type procInhibitor struct {
	name string
	args []string

	mu  sync.Mutex
	cmd *exec.Cmd
}

func newProcInhibitor(name string, args ...string) *procInhibitor {
	return &procInhibitor{name: name, args: args}
}

// Start spawns the inhibitor process if it is not already running.
func (p *procInhibitor) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd != nil {
		return nil // already active
	}

	path, err := exec.LookPath(p.name)
	if err != nil {
		return fmt.Errorf("%s not found: %w", p.name, err)
	}

	cmd := exec.Command(path, p.args...)
	// Run the inhibitor in its own process group so we can signal the whole
	// group on Stop. systemd-inhibit spawns a `sleep infinity` grandchild;
	// killing only the parent would orphan it, so we kill the group instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", p.name, err)
	}
	p.cmd = cmd
	return nil
}

// Stop kills the inhibitor process group and releases the assertion. Safe to
// call when not started.
func (p *procInhibitor) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd == nil || p.cmd.Process == nil {
		p.cmd = nil
		return nil
	}

	pid := p.cmd.Process.Pid
	// Kill the entire process group (negative pid). This reaps any children
	// the inhibitor spawned (e.g. `sleep infinity`) along with the parent.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		// Fall back to killing just the parent if the group signal failed.
		_ = p.cmd.Process.Kill()
	}
	_ = p.cmd.Wait()
	p.cmd = nil
	return nil
}

// Active reports whether the inhibitor process is currently running.
func (p *procInhibitor) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil
}
