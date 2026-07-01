package jobs

import (
	"os"
	"path/filepath"
	"strings"
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

func TestScrubEnv_DropsInheritedSecretsKeepsSafe(t *testing.T) {
	// A synthetic base environment mixing safe vars, inherited secrets, and a
	// contrived locale var that must be denied despite the LC_ prefix.
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/citadel",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"TERM=xterm",
		"USER=citadel",
		"FOO_TOKEN=super-secret",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"AWS_ACCESS_KEY_ID=aws-id",
		"GITHUB_TOKEN=ghp_deadbeef",
		"DOCKER_HOST=tcp://evil",
		"CITADEL_DEVICE_TOKEN=dev-token",
		"MY_PASSWORD=hunter2",
		"SSH_AUTH_SOCK=/tmp/agent.sock",
		"LC_SECRET_KEY=nope",
		"RANDOM_UNLISTED=whatever",
	}

	got := scrubEnv(base, nil)
	set := make(map[string]string)
	for _, kv := range got {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			set[kv[:eq]] = kv[eq+1:]
		}
	}

	// Must be kept.
	wantKept := []string{"PATH", "HOME", "LANG", "LC_ALL", "TERM", "USER"}
	for _, name := range wantKept {
		if _, ok := set[name]; !ok {
			t.Errorf("expected %s to be kept in scrubbed env, but it was dropped", name)
		}
	}

	// Must be dropped: inherited secrets and unlisted vars.
	wantDropped := []string{
		"FOO_TOKEN",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_ACCESS_KEY_ID",
		"GITHUB_TOKEN",
		"DOCKER_HOST",
		"CITADEL_DEVICE_TOKEN",
		"MY_PASSWORD",
		"SSH_AUTH_SOCK",
		"LC_SECRET_KEY", // deny (contains KEY) wins over LC_ allow prefix
		"RANDOM_UNLISTED",
	}
	for _, name := range wantDropped {
		if v, ok := set[name]; ok {
			t.Errorf("expected %s to be scrubbed, but it leaked with value %q", name, v)
		}
	}

	// PATH must be augmented with the standard fallback dirs.
	if !strings.Contains(set["PATH"], "/usr/bin") || !strings.Contains(set["PATH"], "/bin") {
		t.Errorf("PATH not augmented with standard dirs: %q", set["PATH"])
	}
}

func TestScrubEnv_JobProvidedVarsBypassLists(t *testing.T) {
	// Explicit job-provided vars are trusted and forwarded even if their names
	// would otherwise trip the denylist, and they override inherited values.
	base := []string{"PATH=/usr/bin", "HOME=/home/citadel"}
	jobEnv := map[string]string{
		"MY_TOKEN": "explicit",   // denylisted name, but explicitly provided
		"HOME":     "/tmp/custom", // override inherited HOME
	}

	got := scrubEnv(base, jobEnv)
	set := make(map[string]string)
	for _, kv := range got {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			set[kv[:eq]] = kv[eq+1:]
		}
	}

	if set["MY_TOKEN"] != "explicit" {
		t.Errorf("job-provided MY_TOKEN should be forwarded, got %q", set["MY_TOKEN"])
	}
	if set["HOME"] != "/tmp/custom" {
		t.Errorf("job-provided HOME should override inherited value, got %q", set["HOME"])
	}
}

func TestShellCommand_DisabledRefusesAndDoesNotExec(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "executed.marker")

	h := &ShellCommandHandler{Disabled: true}
	out, err := runShell(t, h, "touch "+sentinel)

	if err == nil {
		t.Fatal("expected refusal error when shell is disabled, got nil")
	}
	if err.Error() != ShellDisabledError {
		t.Errorf("error = %q, want %q", err.Error(), ShellDisabledError)
	}
	if len(out) != 0 {
		t.Errorf("expected no output when disabled, got %q", string(out))
	}
	// Proves the command never ran: the sentinel file must not exist.
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("command executed despite shell being disabled (sentinel file was created)")
	}
}

func TestShellCommand_EnabledByDefault(t *testing.T) {
	// A zero-value handler (Disabled=false) must still execute normally.
	h := &ShellCommandHandler{}
	out, err := runShell(t, h, "echo ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "ok\n" {
		t.Errorf("output = %q, want %q", string(out), "ok\n")
	}
}

func TestShellCommand_ScrubsSecretsFromChildEnv(t *testing.T) {
	// End-to-end: a planted secret in the real process env must not be visible
	// to the executed command.
	t.Setenv("FOO_TOKEN", "leak-me")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak-me-too")

	h := &ShellCommandHandler{}
	out, err := runShell(t, h, "echo \"[$FOO_TOKEN][$AWS_SECRET_ACCESS_KEY]\"")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(out)) != "[][]" {
		t.Errorf("secrets leaked into child env: output = %q", string(out))
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
