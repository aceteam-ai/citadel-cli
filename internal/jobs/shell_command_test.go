package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func runShell(t *testing.T, h *ShellCommandHandler, command string) ([]byte, error) {
	t.Helper()
	return h.Execute(JobContext{}, &nexus.Job{
		ID:   "test",
		Type: "SHELL_COMMAND",
		Payload: map[string]string{
			"command": command,
		},
	})
}

func TestShellCommand_MissingCommand(t *testing.T) {
	h := &ShellCommandHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "test-1",
		Type:    "SHELL_COMMAND",
		Payload: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for missing command, got nil")
	}
}

func TestShellCommand_EmptyCommand(t *testing.T) {
	h := &ShellCommandHandler{}
	// Whitespace-only command must be rejected.
	_, err := runShell(t, h, "   ")
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

func TestShellCommand_BasicExecution(t *testing.T) {
	h := &ShellCommandHandler{}
	out, err := runShell(t, h, "echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("output = %q, want %q", string(out), "hello\n")
	}
}

func TestShellCommand_Pipe(t *testing.T) {
	h := &ShellCommandHandler{}
	out, err := runShell(t, h, "echo hello | cat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("output = %q, want %q", string(out), "hello\n")
	}
}

func TestShellCommand_AndAnd(t *testing.T) {
	h := &ShellCommandHandler{}
	out, err := runShell(t, h, "true && echo ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "ok\n" {
		t.Errorf("output = %q, want %q", string(out), "ok\n")
	}
}

func TestShellCommand_QuotedArgs(t *testing.T) {
	h := &ShellCommandHandler{}
	out, err := runShell(t, h, `echo "a b c"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "a b c\n" {
		t.Errorf("output = %q, want %q", string(out), "a b c\n")
	}
}

func TestShellCommand_MultiLineScript(t *testing.T) {
	h := &ShellCommandHandler{}
	out, err := runShell(t, h, "set -e\necho one\necho two")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "one\ntwo\n" {
		t.Errorf("output = %q, want %q", string(out), "one\ntwo\n")
	}
}

func TestShellCommand_NonZeroExit(t *testing.T) {
	h := &ShellCommandHandler{}
	_, err := runShell(t, h, "exit 3")
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
}

func TestShellCommand_WorkspaceCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "foo.txt"), []byte("bar"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	h := NewShellCommandHandler(dir)
	// Relative path resolves against the configured workspace directory.
	out, err := runShell(t, h, "cat foo.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "bar" {
		t.Errorf("output = %q, want %q", string(out), "bar")
	}
}

func TestShellCommand_RestrictedPATH(t *testing.T) {
	// With a restricted inherited PATH, augmentedEnv must still let /bin/sh
	// resolve a non-builtin external binary (uname) via standardPATHDirs.
	origPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPATH) })
	os.Setenv("PATH", "/nonexistent_dir_only")

	h := &ShellCommandHandler{}
	out, err := runShell(t, h, "uname")
	if err != nil {
		t.Fatalf("shell command with restricted PATH should work via augmented env: %v", err)
	}
	if len(out) == 0 {
		t.Error("expected uname output, got empty")
	}
}
