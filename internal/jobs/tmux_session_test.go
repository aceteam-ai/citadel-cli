package jobs

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/tmux"
)

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
