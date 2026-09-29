package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestValidateSessionName(t *testing.T) {
	valid := []string{"agent", "agent-1", "node_console", "ABC123", "a", "x-y_z"}
	for _, name := range valid {
		if err := ValidateSessionName(name); err != nil {
			t.Errorf("ValidateSessionName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",                   // empty
		"has space",          // whitespace
		"has.dot",            // tmux window/pane separator
		"has:colon",          // tmux target separator
		"semi;rm -rf",        // shell metacharacter
		"$(whoami)",          // command substitution
		"name\nwith-newline", // control character
		"../escape",          // path traversal chars
	}
	for _, name := range invalid {
		err := ValidateSessionName(name)
		if err == nil {
			t.Errorf("ValidateSessionName(%q) = nil, want error", name)
			continue
		}
		if !errors.Is(err, ErrInvalidSessionName) {
			t.Errorf("ValidateSessionName(%q) error = %v, want wrapping ErrInvalidSessionName", name, err)
		}
	}
}

func TestValidateSessionName_TooLong(t *testing.T) {
	name := make([]byte, 65)
	for i := range name {
		name[i] = 'a'
	}
	if err := ValidateSessionName(string(name)); err == nil {
		t.Error("expected error for 65-char name, got nil")
	}
	short := name[:64]
	if err := ValidateSessionName(string(short)); err != nil {
		t.Errorf("64-char name should be valid, got %v", err)
	}
}

func TestAttachOrCreateArgs(t *testing.T) {
	got := AttachOrCreateArgs("agent", "/bin/bash")
	want := []string{
		"new-session", "-A", "-s", "agent", "/bin/bash",
		";", "set-option", "-q", "-t", "agent", managedSessionOption, managedSessionOptionValue,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachOrCreateArgs = %v, want %v", got, want)
	}

	// Empty shell omits the trailing program so tmux uses its default.
	got = AttachOrCreateArgs("agent", "")
	want = []string{
		"new-session", "-A", "-s", "agent",
		";", "set-option", "-q", "-t", "agent", managedSessionOption, managedSessionOptionValue,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachOrCreateArgs (no shell) = %v, want %v", got, want)
	}
}

func TestNewDetachedArgs(t *testing.T) {
	got := NewDetachedArgs("agent", "/bin/zsh")
	want := []string{"new-session", "-d", "-s", "agent", "/bin/zsh"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NewDetachedArgs = %v, want %v", got, want)
	}
}

func TestHasSessionArgs(t *testing.T) {
	got := HasSessionArgs("agent")
	want := []string{"has-session", "-t", "agent"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HasSessionArgs = %v, want %v", got, want)
	}
}

func TestListSessionsArgs(t *testing.T) {
	got := ListSessionsArgs()
	want := []string{"list-sessions", "-F", "#{session_name}"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListSessionsArgs = %v, want %v", got, want)
	}
}

func TestParseSessionList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"agent\nconsole\n", []string{"agent", "console"}},
		{"  agent  \n\n console \n", []string{"agent", "console"}},
		{"", nil},
		{"\n\n", nil},
		{"only", []string{"only"}},
	}
	for _, c := range cases {
		got := parseSessionList([]byte(c.in))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseSessionList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// fakeRunner records invocations and returns scripted results keyed by the
// first tmux subcommand, so Manager logic can be tested without a real tmux.
type fakeRunner struct {
	calls   [][]string
	outputs map[string][]byte
	errs    map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	return f.outputs[key], f.errs[key]
}

// exitError returns an *exec.ExitError-typed error, simulating tmux exiting
// non-zero (e.g. has-session for an absent session).
func exitError(t *testing.T) error {
	t.Helper()
	// `false` reliably exits 1 on unix; on other platforms skip the exec-based
	// construction and use a run that is guaranteed to fail.
	cmd := exec.Command("false")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit 1")
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T (%v)", err, err)
	}
	return err
}

func TestManager_HasSession(t *testing.T) {
	f := newFakeRunner()
	m := NewManagerWith("tmux", f)

	// Present: runner returns no error.
	exists, err := m.HasSession(context.Background(), "agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected session to be present")
	}

	// Absent: runner returns a clean non-zero exit.
	f.errs["has-session"] = exitError(t)
	exists, err = m.HasSession(context.Background(), "agent")
	if err != nil {
		t.Fatalf("unexpected error for absent session: %v", err)
	}
	if exists {
		t.Error("expected session to be absent")
	}
}

func TestManager_HasSession_InvalidName(t *testing.T) {
	m := NewManagerWith("tmux", newFakeRunner())
	if _, err := m.HasSession(context.Background(), "bad name"); !errors.Is(err, ErrInvalidSessionName) {
		t.Errorf("expected ErrInvalidSessionName, got %v", err)
	}
}

func TestManager_EnsureSession_Idempotent(t *testing.T) {
	f := newFakeRunner()
	m := NewManagerWith("tmux", f)

	// Session already exists -> EnsureSession must not call new-session.
	if err := m.EnsureSession(context.Background(), "agent", "/bin/bash"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "new-session" {
			t.Fatalf("EnsureSession created a session that already existed: %v", c)
		}
	}
	if got := f.calls[len(f.calls)-1]; !reflect.DeepEqual(got, MarkSessionArgs("agent")) {
		t.Fatalf("existing session was not adopted as managed: %v", got)
	}
}

func TestManager_EnsureSession_CreatesWhenAbsent(t *testing.T) {
	f := newFakeRunner()
	f.errs["has-session"] = exitError(t) // absent
	m := NewManagerWith("tmux", f)

	if err := m.EnsureSession(context.Background(), "agent", "/bin/bash"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var created bool
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "new-session" {
			created = true
			want := NewDetachedArgs("agent", "/bin/bash")
			if !reflect.DeepEqual(c, want) {
				t.Errorf("new-session args = %v, want %v", c, want)
			}
		}
	}
	if !created {
		t.Error("EnsureSession did not create the absent session")
	}
}

func TestManager_EnsureSessionUsesScopeCommand(t *testing.T) {
	f := newFakeRunner()
	f.errs["has-session"] = exitError(t)
	m := &Manager{
		bin:    "/usr/bin/tmux",
		runner: f,
		scopeCommand: func(_ string, command []string) []string {
			return append([]string{"systemd-run", "--scope", "--"}, command...)
		},
	}

	if err := m.EnsureSession(context.Background(), "agent", "/bin/bash"); err != nil {
		t.Fatalf("EnsureSession() error: %v", err)
	}
	want := []string{"--scope", "--", "/usr/bin/tmux", "new-session", "-d", "-s", "agent", "/bin/bash"}
	if got := f.calls[len(f.calls)-2]; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped create args = %v, want %v", got, want)
	}
	if got := f.calls[len(f.calls)-1]; !reflect.DeepEqual(got, MarkSessionArgs("agent")) {
		t.Fatalf("mark args = %v, want %v", got, MarkSessionArgs("agent"))
	}
}

type reaperRunner struct {
	calls [][]string
	now   time.Time
}

func (r *reaperRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "list-sessions":
		return []byte(strings.Join([]string{
			fmt.Sprintf("expired\t%d\t0\t%s", r.now.Add(-8*time.Hour).Unix(), managedSessionOptionValue),
			fmt.Sprintf("became-attached\t%d\t0\t%s", r.now.Add(-8*time.Hour).Unix(), managedSessionOptionValue),
			fmt.Sprintf("recent\t%d\t0\t%s", r.now.Add(-time.Hour).Unix(), managedSessionOptionValue),
			fmt.Sprintf("attached\t%d\t1\t%s", r.now.Add(-8*time.Hour).Unix(), managedSessionOptionValue),
			fmt.Sprintf("operator\t%d\t0\t", r.now.Add(-8*time.Hour).Unix()),
			"malformed\tnot-a-time\t0\t" + managedSessionOptionValue,
		}, "\n") + "\n"), nil
	case "display-message":
		name := args[3]
		attached := 0
		if name == "became-attached" {
			attached = 1
		}
		return []byte(fmt.Sprintf("%s\t%d\t%d\t%s\n", name, r.now.Add(-8*time.Hour).Unix(), attached, managedSessionOptionValue)), nil
	case "kill-session":
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command: %v", args)
	}
}

func TestManager_ReapIdleSessionsOnlyKillsManagedDetachedExpired(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	runner := &reaperRunner{now: now}
	manager := NewManagerWith("tmux", runner)

	reaped, err := manager.ReapIdleSessions(context.Background(), now.Add(-7*time.Hour))
	if err != nil {
		t.Fatalf("ReapIdleSessions() error: %v", err)
	}
	if want := []string{"expired"}; !reflect.DeepEqual(reaped, want) {
		t.Fatalf("reaped = %v, want %v", reaped, want)
	}

	var killed []string
	for _, call := range runner.calls {
		if len(call) >= 3 && call[0] == "kill-session" {
			killed = append(killed, call[2])
		}
	}
	if want := []string{"expired"}; !reflect.DeepEqual(killed, want) {
		t.Fatalf("kill targets = %v, want %v; calls=%v", killed, want, runner.calls)
	}
}

func TestParseSessionStatusesFailsClosed(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	out := fmt.Sprintf("managed\t%d\t0\t%s\noperator\t%d\t0\t\nbad name\t%d\t0\t%s\n", now.Unix(), managedSessionOptionValue, now.Unix(), now.Unix(), managedSessionOptionValue)
	got := parseSessionStatuses([]byte(out))
	want := []SessionStatus{
		{Name: "managed", LastActivity: now, Managed: true},
		{Name: "operator", LastActivity: now},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSessionStatuses() = %#v, want %#v", got, want)
	}
}

func TestManager_ListSessions(t *testing.T) {
	f := newFakeRunner()
	f.outputs["list-sessions"] = []byte("agent\nconsole\n")
	m := NewManagerWith("tmux", f)

	got, err := m.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"agent", "console"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListSessions = %v, want %v", got, want)
	}
}

func TestManager_ListSessions_NoServer(t *testing.T) {
	f := newFakeRunner()
	f.errs["list-sessions"] = exitError(t) // tmux: no server running
	m := NewManagerWith("tmux", f)

	got, err := m.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("no server running should yield empty list, got error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty list, got %v", got)
	}
}

func TestResolve_OverrideMissing(t *testing.T) {
	t.Setenv(envTmuxBin, filepath.Join(t.TempDir(), "does-not-exist"))
	_, err := Resolve()
	if !errors.Is(err, ErrTmuxNotFound) {
		t.Errorf("expected ErrTmuxNotFound for missing override, got %v", err)
	}
}

func TestResolve_OverridePresent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(envTmuxBin, bin)
	got, err := Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bin {
		t.Errorf("Resolve() = %q, want %q", got, bin)
	}
}

func TestManagedBinaryPath(t *testing.T) {
	got := ManagedBinaryPath()
	if got == "" {
		t.Fatal("ManagedBinaryPath returned empty")
	}
	base := filepath.Base(got)
	wantBase := "tmux"
	if runtime.GOOS == "windows" {
		wantBase = "tmux.exe"
	}
	if base != wantBase {
		t.Errorf("ManagedBinaryPath base = %q, want %q", base, wantBase)
	}
}
