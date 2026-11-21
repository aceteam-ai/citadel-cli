package laneinventory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

// Explicitly opt in to a disposable private socket, never the host server.
// Only these fixture PIDs are observed; NewNative/host enumeration is not run.
func TestInventoryRealTmuxFraming(t *testing.T) {
	if os.Getenv("CITADEL_INVENTORY_TMUX_IT") != "1" || runtime.GOOS == "windows" {
		t.Skip("private tmux fixture not requested")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("tmux fixture requested but executable unavailable")
	}
	// Keep the Unix socket pathname below the kernel limit even when Go's
	// TMPDIR points at a deeply nested lane cache. This directory is ours only.
	root, err := os.MkdirTemp("/tmp", "citadel-inventory-tmux-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(root, "fixture.sock")
	cwd := filepath.Join(root, "crafted\n999|888|@1|%99|/dev/pts/7|0\n")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, bin, append([]string{"-S", socket, "-f", os.DevNull, name}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "XDG_CONFIG_HOME=" + root, "TERM=xterm-256color", "SHELL=/bin/sh"}
		cmd.WaitDelay = 100 * time.Millisecond
		out, err := cmd.CombinedOutput()
		return out, err
	}
	created := false
	t.Cleanup(func() {
		if !created {
			return
		}
		stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if _, err := run(stopCtx, "kill-server"); err != nil {
			t.Error("private fixture server cleanup failed")
		}
	})
	if _, err := run(ctx, "new-session", "-d", "-s", "fixture-owner", "-c", cwd, "/bin/sh"); err != nil {
		t.Fatal("private fixture creation failed", err)
	}
	created = true
	if _, err := run(ctx, "new-session", "-d", "-s", "custom-fixture-terminal", "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, "set-option", "-t", "custom-fixture-terminal", "@citadel_managed", "citadel-v1"); err != nil {
		t.Fatal(err)
	}
	// tmux 3.4 emits the old control delimiter as the printable four-byte
	// sequence \\037. The new identity rows must work with actual pipe output.
	legacy, err := run(ctx, "-N", "list-panes", "-a", "-F", "#{pane_id}\x1f#{pane_pid}")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("legacy control delimiter escaped by installed tmux: %v", strings.Contains(string(legacy), `\037`))
	panes, err := readPanes(ctx, run, "")
	if err != nil || len(panes) != 2 {
		t.Fatalf("real tmux framing: count=%d error=%v", len(panes), err)
	}
	owner, terminal := false, false
	for _, pane := range panes {
		if pane.PID == 888 || pane.Target == "%99" {
			t.Fatal("crafted cwd forged an identity row")
		}
		p, err := process.NewProcessWithContext(ctx, pane.PID)
		if err != nil {
			t.Fatal(err)
		}
		parent, err := p.PpidWithContext(ctx)
		if err != nil || parent != pane.ServerPID {
			t.Fatal("pane root is not the recorded server's child")
		}
		observed, err := nativeProcesses(ctx, int32(os.Geteuid()), func(context.Context) ([]processRef, error) {
			return []processRef{{pid: pane.PID, reader: p}}, nil
		})
		if err != nil || len(observed) != 1 || observed[0].TTY != pane.TTY || observed[0].StartedAt <= 0 {
			t.Fatal("fixture kernel TTY/birth observation does not match tmux identity")
		}
		if pane.Session == "fixture-owner" {
			// tmux may sanitize control characters in this format's value. The
			// value is never reparsed as identities, whatever its representation.
			owner = !pane.Terminal && !strings.ContainsAny(pane.Cwd, "\r\n\t")
		}
		if pane.Session == "custom-fixture-terminal" {
			terminal = pane.Terminal
		}
	}
	if !owner || !terminal {
		t.Fatal("real metadata lost cwd framing or terminal marker")
	}
}
