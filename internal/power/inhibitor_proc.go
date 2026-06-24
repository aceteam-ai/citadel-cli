//go:build darwin || linux

package power

import (
	"fmt"
	"os/exec"
	"sync"
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
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", p.name, err)
	}
	p.cmd = cmd
	return nil
}

// Stop kills the inhibitor process and releases the assertion. Safe to call
// when not started.
func (p *procInhibitor) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cmd == nil || p.cmd.Process == nil {
		p.cmd = nil
		return nil
	}

	err := p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	p.cmd = nil
	return err
}

// Active reports whether the inhibitor process is currently running.
func (p *procInhibitor) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil
}
