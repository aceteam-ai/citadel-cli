package terminal

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/tmux"
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

func TestSessionCommand_DisableSentinel(t *testing.T) {
	makeFakeTmux(t)
	for _, name := range []string{"none", "off", "OFF", "disabled", "false", "0", " none "} {
		if got := sessionCommand(name, "/bin/bash"); got != nil {
			t.Errorf("sessionCommand(%q) = %v, want nil (disabled)", name, got)
		}
	}
}

func TestSessionDisabled(t *testing.T) {
	on := []string{"", "citadel", "agent", "my-session", "nonexistent"}
	off := []string{"none", "off", "OFF", "Disabled", "false", "0", "  off  "}
	for _, s := range on {
		if sessionDisabled(s) {
			t.Errorf("sessionDisabled(%q) = true, want false", s)
		}
	}
	for _, s := range off {
		if !sessionDisabled(s) {
			t.Errorf("sessionDisabled(%q) = false, want true", s)
		}
	}
}

func TestSessionNameForUser(t *testing.T) {
	// Empty user falls back to the base name (shared but persistent).
	if got := sessionNameForUser("citadel", ""); got != "citadel" {
		t.Errorf("empty user: got %q, want %q", got, "citadel")
	}

	// A tmux-safe user id (e.g. a UUID without dashes, or alnum) is appended
	// verbatim.
	if got := sessionNameForUser("citadel", "user123"); got != "citadel-user123" {
		t.Errorf("safe user: got %q, want %q", got, "citadel-user123")
	}

	// Deterministic: same inputs yield the same name (reconnect key stability).
	a := sessionNameForUser("citadel", "alice@example.com")
	b := sessionNameForUser("citadel", "alice@example.com")
	if a != b {
		t.Errorf("not deterministic: %q != %q", a, b)
	}

	// Distinct users that sanitise to the same cleaned string must not collide.
	x := sessionNameForUser("citadel", "a@b.com")
	y := sessionNameForUser("citadel", "ab.com")
	if x == y {
		t.Errorf("collision: both users -> %q", x)
	}

	// Every derived name must be a valid tmux session name.
	for _, uid := range []string{
		"user123",
		"alice@example.com",
		"00000000-0000-0000-0000-000000000000",
		strings.Repeat("x", 200),
		"@@@@",
	} {
		name := sessionNameForUser("citadel", uid)
		if err := tmux.ValidateSessionName(name); err != nil {
			t.Errorf("derived name %q for uid %q invalid: %v", name, uid, err)
		}
	}
}
