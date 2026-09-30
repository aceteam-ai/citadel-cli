package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/tmux"
)

type fakeTmuxSessionManager struct {
	leaseUntil time.Time
}

func (f *fakeTmuxSessionManager) ListSessions(context.Context) ([]string, error) { return nil, nil }
func (f *fakeTmuxSessionManager) HasSession(context.Context, string) (bool, error) {
	return false, nil
}
func (f *fakeTmuxSessionManager) EnsureSessionLease(_ context.Context, _, _ string, leaseUntil time.Time) error {
	f.leaseUntil = leaseUntil
	return nil
}

func tmuxJob(payload map[string]string) *nexus.Job {
	return &nexus.Job{ID: "test", Type: "TMUX_SESSION", Payload: payload}
}

func TestTmuxSessionHandler_Unavailable(t *testing.T) {
	// Point the resolver at a nonexistent override so tmux cannot be found,
	// regardless of whether the runner has tmux on PATH.
	t.Setenv("CITADEL_TMUX_BIN", filepath.Join(t.TempDir(), "nope"))

	h := NewTmuxSessionHandler("")
	_, err := h.Execute(JobContext{}, tmuxJob(map[string]string{"action": "list"}))
	if err == nil {
		t.Fatal("expected error when tmux is unavailable, got nil")
	}
	if !errors.Is(err, tmux.ErrTmuxNotFound) {
		t.Errorf("expected ErrTmuxNotFound, got %v", err)
	}
}

func TestTmuxSessionHandler_UnsupportedAction(t *testing.T) {
	if !tmux.IsAvailable() {
		t.Skip("tmux not available on this runner")
	}
	h := NewTmuxSessionHandler("")
	_, err := h.Execute(JobContext{}, tmuxJob(map[string]string{"action": "bogus"}))
	if err == nil {
		t.Fatal("expected error for unsupported action, got nil")
	}
}

func TestTmuxSessionHandler_InvalidName(t *testing.T) {
	if !tmux.IsAvailable() {
		t.Skip("tmux not available on this runner")
	}
	h := NewTmuxSessionHandler("")
	_, err := h.Execute(JobContext{}, tmuxJob(map[string]string{"action": "ensure", "name": "bad name"}))
	if err == nil {
		t.Fatal("expected error for invalid session name, got nil")
	}
	if !errors.Is(err, tmux.ErrInvalidSessionName) {
		t.Errorf("expected ErrInvalidSessionName, got %v", err)
	}
}

func TestTmuxSessionHandler_ListEnvelope(t *testing.T) {
	if !tmux.IsAvailable() {
		t.Skip("tmux not available on this runner")
	}
	h := NewTmuxSessionHandler("")
	out, err := h.Execute(JobContext{}, tmuxJob(map[string]string{"action": "list"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res tmuxSessionResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, out)
	}
	if res.Action != "list" {
		t.Errorf("action = %q, want %q", res.Action, "list")
	}
}

func TestTmuxSessionHandler_EnsureHonorsTerminalSessionTTL(t *testing.T) {
	t.Setenv(tmux.EnvSessionLeaseTTL, "90m")
	now := time.Unix(2_000_000_000, 0)
	mgr := &fakeTmuxSessionManager{}
	h := NewTmuxSessionHandler("")
	h.now = func() time.Time { return now }
	h.newManager = func() (tmuxSessionManager, error) { return mgr, nil }

	if _, err := h.Execute(JobContext{}, tmuxJob(map[string]string{"action": "ensure", "name": "agent"})); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if want := now.Add(90 * time.Minute); !mgr.leaseUntil.Equal(want) {
		t.Fatalf("lease deadline = %v, want %v", mgr.leaseUntil, want)
	}
}

func TestTmuxSessionHandler_EnsureHonorsDisabledExpiry(t *testing.T) {
	t.Setenv(tmux.EnvSessionLeaseTTL, "0")
	mgr := &fakeTmuxSessionManager{}
	h := NewTmuxSessionHandler("")
	h.newManager = func() (tmuxSessionManager, error) { return mgr, nil }

	if _, err := h.Execute(JobContext{}, tmuxJob(map[string]string{"action": "create", "name": "agent"})); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !mgr.leaseUntil.IsZero() {
		t.Fatalf("disabled lease deadline = %v, want zero", mgr.leaseUntil)
	}
}

func TestTmuxSessionHandler_EnsureValidatesTerminalSessionTTL(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	for _, tc := range []struct {
		name      string
		value     string
		wantLease time.Time
		wantErr   bool
	}{
		{name: "minimum", value: "1m", wantLease: now.Add(time.Minute)},
		{name: "disabled", value: "0", wantLease: time.Time{}},
		{name: "below floor", value: "30s", wantErr: true},
		{name: "negative", value: "-1m", wantErr: true},
		{name: "malformed", value: "not-a-duration", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tmux.EnvSessionLeaseTTL, tc.value)
			mgr := &fakeTmuxSessionManager{}
			h := NewTmuxSessionHandler("")
			h.now = func() time.Time { return now }
			managerCalls := 0
			h.newManager = func() (tmuxSessionManager, error) {
				managerCalls++
				return mgr, nil
			}

			_, err := h.Execute(JobContext{}, tmuxJob(map[string]string{"action": "ensure", "name": "agent"}))
			if tc.wantErr {
				if !errors.Is(err, tmux.ErrInvalidSessionLeaseTTL) {
					t.Fatalf("Execute() error = %v, want ErrInvalidSessionLeaseTTL", err)
				}
				if !mgr.leaseUntil.IsZero() {
					t.Fatalf("invalid TTL reached manager with deadline %v", mgr.leaseUntil)
				}
				if managerCalls != 0 {
					t.Fatalf("invalid TTL constructed tmux manager %d time(s), want fail before tmux access", managerCalls)
				}
				return
			}
			if err != nil || !mgr.leaseUntil.Equal(tc.wantLease) {
				t.Fatalf("Execute() = deadline %v, error %v; want %v, nil", mgr.leaseUntil, err, tc.wantLease)
			}
			if managerCalls != 1 {
				t.Fatalf("valid TTL constructed tmux manager %d time(s), want 1", managerCalls)
			}
		})
	}
}
