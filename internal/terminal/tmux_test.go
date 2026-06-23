package terminal

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// makeFakeTmux writes an executable stub and points CITADEL_TMUX_BIN at it so
// tmux.Resolve succeeds deterministically without a real tmux on the runner.
func makeFakeTmux(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("setup fake tmux: %v", err)
	}
	t.Setenv("CITADEL_TMUX_BIN", bin)
	return bin
}

func TestSessionCommand_NoSessionName(t *testing.T) {
	if got := sessionCommand("", "/bin/bash"); got != nil {
		t.Errorf("expected nil command without a session name, got %v", got)
	}
}

func TestSessionCommand_InvalidName(t *testing.T) {
	makeFakeTmux(t)
	if got := sessionCommand("bad name", "/bin/bash"); got != nil {
		t.Errorf("expected nil command for invalid session name, got %v", got)
	}
}

func TestSessionCommand_TmuxUnavailable(t *testing.T) {
	// Override points at a nonexistent file -> Resolve fails -> fall back.
	t.Setenv("CITADEL_TMUX_BIN", filepath.Join(t.TempDir(), "missing"))
	if got := sessionCommand("agent", "/bin/bash"); got != nil {
		t.Errorf("expected nil command when tmux is unavailable, got %v", got)
	}
}

func TestSessionCommand_BuildsAttachOrCreate(t *testing.T) {
	bin := makeFakeTmux(t)
	got := sessionCommand("agent", "/bin/bash")
	want := []string{bin, "new-session", "-A", "-s", "agent", "/bin/bash"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionCommand = %v, want %v", got, want)
	}
}
