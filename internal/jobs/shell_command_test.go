package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// allowPasscode is a test verifier that accepts any presented passcode. It lets
// the pre-existing shell tests exercise the ENABLED execution path without
// threading a real node passcode through each case; the passcode gate itself is
// covered by the dedicated TestShellCommand_Passcode* cases below.
func allowPasscode(string) bool { return true }

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
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
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
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	// Whitespace-only command must be rejected.
	_, err := runShell(t, h, "   ")
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

func TestShellCommand_BasicExecution(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShell(t, h, "echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("output = %q, want %q", string(out), "hello\n")
	}
}

func TestShellCommand_Pipe(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShell(t, h, "echo hello | cat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("output = %q, want %q", string(out), "hello\n")
	}
}

func TestShellCommand_AndAnd(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShell(t, h, "true && echo ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "ok\n" {
		t.Errorf("output = %q, want %q", string(out), "ok\n")
	}
}

func TestShellCommand_QuotedArgs(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShell(t, h, `echo "a b c"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "a b c\n" {
		t.Errorf("output = %q, want %q", string(out), "a b c\n")
	}
}

func TestShellCommand_MultiLineScript(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShell(t, h, "set -e\necho one\necho two")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "one\ntwo\n" {
		t.Errorf("output = %q, want %q", string(out), "one\ntwo\n")
	}
}

func TestShellCommand_NonZeroExit(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
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
	h.VerifyPasscode = allowPasscode
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
		"MY_TOKEN": "explicit",    // denylisted name, but explicitly provided
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
	// An enabled handler (Disabled=false) with a satisfied passcode gate executes
	// normally.
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
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

	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShell(t, h, "echo \"[$FOO_TOKEN][$AWS_SECRET_ACCESS_KEY]\"")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(out)) != "[][]" {
		t.Errorf("secrets leaked into child env: output = %q", string(out))
	}
}

// runShellWithPasscode dispatches a SHELL_COMMAND carrying a passcode payload
// field, exercising the aceteam#6524 per-job passcode gate.
func runShellWithPasscode(t *testing.T, h *ShellCommandHandler, command, passcode string) ([]byte, error) {
	t.Helper()
	return h.Execute(JobContext{}, &nexus.Job{
		ID:   "test",
		Type: "SHELL_COMMAND",
		Payload: map[string]string{
			"command":               command,
			ShellPasscodePayloadKey: passcode,
		},
	})
}

// TestShellCommand_EnabledNilVerifierFailsClosed proves the fail-closed contract
// at the type level: an ENABLED handler (Disabled=false) whose passcode verifier
// was never wired refuses every command, so a forgotten gate never silently opens
// root shell.
func TestShellCommand_EnabledNilVerifierFailsClosed(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "executed.marker")

	h := &ShellCommandHandler{} // enabled, VerifyPasscode nil
	out, err := runShell(t, h, "touch "+sentinel)

	if err == nil {
		t.Fatal("expected refusal when the passcode gate is not wired, got nil")
	}
	if err.Error() != ShellPasscodeRequiredError {
		t.Errorf("error = %q, want %q", err.Error(), ShellPasscodeRequiredError)
	}
	if len(out) != 0 {
		t.Errorf("expected no output, got %q", string(out))
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("command executed despite a nil passcode verifier")
	}
}

// TestShellCommand_EnabledWrongPasscodeRefused proves that an enabled handler
// with a real verifier still refuses a command that presents the wrong (or no)
// passcode, and does not execute it.
func TestShellCommand_EnabledWrongPasscodeRefused(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "executed.marker")

	// Verifier that only accepts the correct PIN.
	h := &ShellCommandHandler{VerifyPasscode: func(pin string) bool { return pin == "2468" }}

	// Wrong passcode.
	out, err := runShellWithPasscode(t, h, "touch "+sentinel, "0000")
	if err == nil || err.Error() != ShellPasscodeRequiredError {
		t.Fatalf("wrong passcode: err = %v, want %q", err, ShellPasscodeRequiredError)
	}
	// Absent passcode.
	if _, err := runShell(t, h, "touch "+sentinel); err == nil || err.Error() != ShellPasscodeRequiredError {
		t.Fatalf("absent passcode: err = %v, want %q", err, ShellPasscodeRequiredError)
	}
	if len(out) != 0 {
		t.Errorf("expected no output, got %q", string(out))
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("command executed despite a wrong/absent passcode")
	}
}

// TestShellCommand_EnabledCorrectPasscodeRuns proves the allow path: an enabled
// handler runs the command when the correct passcode is presented.
func TestShellCommand_EnabledCorrectPasscodeRuns(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: func(pin string) bool { return pin == "2468" }}
	out, err := runShellWithPasscode(t, h, "echo ok", "2468")
	if err != nil {
		t.Fatalf("correct passcode should run: %v", err)
	}
	if string(out) != "ok\n" {
		t.Errorf("output = %q, want %q", string(out), "ok\n")
	}
}

// TestShellCommand_DisabledBeatsPasscode confirms the Disabled kill-switch is
// checked first: a disabled handler refuses with ShellDisabledError even when a
// correct passcode is presented.
func TestShellCommand_DisabledBeatsPasscode(t *testing.T) {
	h := &ShellCommandHandler{Disabled: true, VerifyPasscode: allowPasscode}
	_, err := runShellWithPasscode(t, h, "echo ok", "2468")
	if err == nil || err.Error() != ShellDisabledError {
		t.Fatalf("disabled handler: err = %v, want %q", err, ShellDisabledError)
	}
}

// TestShellCommand_PasscodeNotForwardedToChildEnv confirms the passcode payload
// field never leaks into the executed command's environment.
func TestShellCommand_PasscodeNotForwardedToChildEnv(t *testing.T) {
	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShellWithPasscode(t, h, "env", "super-secret-pin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(out), "super-secret-pin") {
		t.Errorf("passcode leaked into child environment: %q", string(out))
	}
}

func TestShellCommand_RestrictedPATH(t *testing.T) {
	// With a restricted inherited PATH, augmentedEnv must still let /bin/sh
	// resolve a non-builtin external binary (uname) via standardPATHDirs.
	origPATH := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPATH) })
	os.Setenv("PATH", "/nonexistent_dir_only")

	h := &ShellCommandHandler{VerifyPasscode: allowPasscode}
	out, err := runShell(t, h, "uname")
	if err != nil {
		t.Fatalf("shell command with restricted PATH should work via augmented env: %v", err)
	}
	if len(out) == 0 {
		t.Error("expected uname output, got empty")
	}
}
