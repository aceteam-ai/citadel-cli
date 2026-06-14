package jobs

import (
	"os"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

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
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-2",
		Type: "SHELL_COMMAND",
		Payload: map[string]string{
			"command": "   ",
		},
	})
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

func TestShellCommand_BasicExecution(t *testing.T) {
	h := &ShellCommandHandler{}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-3",
		Type: "SHELL_COMMAND",
		Payload: map[string]string{
			"command": "echo hello",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("output = %q, want %q", string(out), "hello\n")
	}
}

func TestResolveExecutable_NormalPATH(t *testing.T) {
	// "true" is a standard executable in /usr/bin or /bin.
	p, err := resolveExecutable("true")
	if err != nil {
		t.Fatalf("resolveExecutable(true) failed: %v", err)
	}
	if p == "" {
		t.Error("resolved path should not be empty")
	}
}

func TestResolveExecutable_RestrictedPATH(t *testing.T) {
	// Temporarily set PATH to something that excludes standard dirs.
	origPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPATH) })

	os.Setenv("PATH", "/nonexistent_dir_only")

	// "true" lives in /usr/bin or /bin, which are in standardPATHDirs.
	p, err := resolveExecutable("true")
	if err != nil {
		t.Fatalf("resolveExecutable(true) with restricted PATH should still find it via fallback, got: %v", err)
	}
	if p != "/usr/bin/true" && p != "/bin/true" {
		t.Errorf("expected /usr/bin/true or /bin/true, got %q", p)
	}
}

func TestResolveExecutable_NotFound(t *testing.T) {
	_, err := resolveExecutable("definitely_not_a_real_binary_xyz123")
	if err == nil {
		t.Fatal("expected error for nonexistent binary, got nil")
	}
}

func TestResolveExecutable_WithSlash(t *testing.T) {
	// If the name contains a slash, it should be returned as-is.
	p, err := resolveExecutable("/some/absolute/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != "/some/absolute/path" {
		t.Errorf("expected /some/absolute/path, got %q", p)
	}
}

func TestShellCommand_RestrictedPATH(t *testing.T) {
	// Verify the full handler works even with a restricted PATH.
	origPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPATH) })

	os.Setenv("PATH", "/nonexistent_dir_only")

	h := &ShellCommandHandler{}
	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:   "test-4",
		Type: "SHELL_COMMAND",
		Payload: map[string]string{
			"command": "echo works",
		},
	})
	if err != nil {
		t.Fatalf("shell command with restricted PATH should work via fallback: %v", err)
	}
	if string(out) != "works\n" {
		t.Errorf("output = %q, want %q", string(out), "works\n")
	}
}
