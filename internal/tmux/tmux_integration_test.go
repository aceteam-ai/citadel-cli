package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

type socketRunner struct {
	socket string
}

func (r socketRunner) Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	global := []string{"-L", r.socket, "-f", "/dev/null"}
	cmd := exec.CommandContext(ctx, bin, append(global, args...)...)
	cmd.Env = append(os.Environ(), "TMUX=")
	return cmd.CombinedOutput()
}

func TestRealTmuxSocketOwnershipAndReaper(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("real tmux integration skipped: tmux is not installed")
	}
	if err := probeTmuxVersion(bin); err != nil {
		if errors.Is(err, ErrTmuxVersionUnsupported) {
			t.Skipf("real tmux integration skipped: %v", err)
		}
		t.Fatalf("probe tmux version: %v", err)
	}

	runner := socketRunner{socket: fmt.Sprintf("citadel-1185-%d", os.Getpid())}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() {
		_, _ = runner.Run(context.Background(), bin, "kill-server")
	})
	manager := NewManagerWith(bin, runner)
	now := time.Now().Truncate(time.Second)

	if err := manager.EnsureSessionLease(ctx, "citadel-expired", "", now.Add(-time.Minute)); err != nil {
		t.Fatalf("create expired Citadel session: %v", err)
	}
	if err := manager.EnsureSessionLease(ctx, "citadel-future", "", now.Add(time.Hour)); err != nil {
		t.Fatalf("create future Citadel session: %v", err)
	}
	if out, err := runner.Run(ctx, bin, "new-session", "-d", "-s", "operator"); err != nil {
		t.Fatalf("create unmarked operator session: %v: %s", err, out)
	}

	if err := manager.EnsureSessionLease(ctx, "operator", "", now.Add(time.Hour)); !errors.Is(err, ErrSessionNameCollision) {
		t.Fatalf("operator collision error = %v, want ErrSessionNameCollision", err)
	}
	reaped, err := manager.ReapExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("ReapExpiredSessions() error: %v", err)
	}
	if want := []string{"citadel-expired"}; !reflect.DeepEqual(reaped, want) {
		t.Fatalf("reaped = %v, want %v", reaped, want)
	}
	for name, want := range map[string]bool{
		"citadel-expired": false,
		"citadel-future":  true,
		"operator":        true,
	} {
		exists, err := manager.HasSession(ctx, name)
		if err != nil {
			t.Fatalf("HasSession(%q): %v", name, err)
		}
		if exists != want {
			t.Fatalf("HasSession(%q) = %v, want %v", name, exists, want)
		}
	}
}
