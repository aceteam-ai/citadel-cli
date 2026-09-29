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
	"strconv"
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

func TestAttachArgsNeverCreatesOrMarks(t *testing.T) {
	got := AttachArgs("agent")
	want := []string{"attach-session", "-t", "agent"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachArgs = %v, want %v", got, want)
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
	run     func(context.Context, []string) ([]byte, error)
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{outputs: map[string][]byte{}, errs: map[string]error{}}
}

func (f *fakeRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	call := append([]string(nil), args...)
	f.calls = append(f.calls, call)
	if f.run != nil {
		return f.run(ctx, call)
	}
	key := ""
	if len(args) > 0 {
		key = args[0]
	}
	if containsArg(call, "new-session") && f.errs[key] == nil {
		delete(f.errs, "display-message")
		lease := call[len(call)-1]
		f.outputs["display-message"] = []byte("agent\t$1\t42\t0\t" + managedSessionOptionValue + "\t" + lease + "\n")
	}
	if key == "if-shell" && len(call) > 5 && strings.HasPrefix(call[5], "set-option ") {
		fields := strings.Fields(call[5])
		lease := fields[5]
		statusFields := strings.Split(strings.TrimSpace(string(f.outputs["display-message"])), "\t")
		if len(statusFields) == 6 {
			statusFields[5] = lease
			f.outputs["display-message"] = []byte(strings.Join(statusFields, "\t") + "\n")
		}
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

func TestManager_EnsureSessionExistingManagedRenewsWithoutCreate(t *testing.T) {
	f := newFakeRunner()
	f.outputs["display-message"] = []byte("agent\t$1\t42\t0\t" + managedSessionOptionValue + "\t100\n")
	m := NewManagerWith("tmux", f)

	deadline := time.Unix(2_000_000_000, 0)
	if err := m.EnsureSessionLease(context.Background(), "agent", "/bin/bash", deadline); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "new-session" {
			t.Fatalf("EnsureSession created a session that already existed: %v", c)
		}
	}
	if got := f.calls[1]; got[0] != "if-shell" || !strings.Contains(strings.Join(got, " "), "$1") || !strings.Contains(strings.Join(got, " "), "2000000000") {
		t.Fatalf("identity-bound renew args = %v", got)
	}
}

func TestManager_EnsureSessionReplacementDuringRenewFailsClosed(t *testing.T) {
	f := newFakeRunner()
	step := 0
	f.run = func(_ context.Context, args []string) ([]byte, error) {
		step++
		switch step {
		case 1:
			return []byte("agent\t$7\t42\t0\t" + managedSessionOptionValue + "\t100\n"), nil
		case 2:
			if args[0] != "if-shell" || !strings.Contains(args[4], "$7") || !strings.Contains(args[4], "42") {
				t.Fatalf("renew was not identity-bound: %v", args)
			}
			return nil, nil // condition became false after replacement
		case 3:
			return []byte("agent\t$0\t99\t0\t\t0\n"), nil
		default:
			return nil, fmt.Errorf("unexpected call: %v", args)
		}
	}
	m := NewManagerWith("tmux", f)
	err := m.EnsureSessionLease(context.Background(), "agent", "/bin/bash", time.Unix(2_000_000_000, 0))
	if !errors.Is(err, ErrSessionNameCollision) {
		t.Fatalf("error = %v, want ErrSessionNameCollision", err)
	}
}

func TestManager_PrepareSessionAttachIsIdentityBound(t *testing.T) {
	f := newFakeRunner()
	f.outputs["display-message"] = []byte("agent\t$7\t42\t0\t" + managedSessionOptionValue + "\t100\n")
	m := NewManagerWith("tmux", f)
	command, err := m.PrepareSession(context.Background(), "agent", "/bin/bash", time.Unix(2_000_000_000, 0))
	if err != nil {
		t.Fatalf("PrepareSession() error: %v", err)
	}
	joined := strings.Join(command, " ")
	for _, want := range []string{"if-shell", "#{session_id},$7", "#{pid},42", managedSessionOption, "attach-session -t $7"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("identity-bound attach %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "attach-session -t agent") {
		t.Fatalf("attach reused mutable name: %q", joined)
	}
}

func TestManager_EnsureSessionRejectsUnmarkedCollisionWithoutMutation(t *testing.T) {
	f := newFakeRunner()
	f.outputs["display-message"] = []byte("agent\t$1\t42\t0\t\t0\n")
	m := NewManagerWith("tmux", f)

	err := m.EnsureSessionLease(context.Background(), "agent", "/bin/bash", time.Unix(2_000_000_000, 0))
	if !errors.Is(err, ErrSessionNameCollision) {
		t.Fatalf("error = %v, want ErrSessionNameCollision", err)
	}
	if len(f.calls) != 1 || f.calls[0][0] != "display-message" {
		t.Fatalf("operator session was mutated: calls=%v", f.calls)
	}
}

func TestManager_EnsureSessionCreatesThenMarksLease(t *testing.T) {
	f := newFakeRunner()
	f.errs["display-message"] = exitError(t)
	m := NewManagerWith("tmux", f)

	deadline := time.Unix(2_000_000_000, 0)
	if err := m.EnsureSessionLease(context.Background(), "agent", "/bin/bash", deadline); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("create and ownership marker must be one invocation: calls=%v", f.calls)
	}
	if got, want := f.calls[1], NewManagedDetachedArgs("agent", "/bin/bash", deadline.Unix()); !reflect.DeepEqual(got, want) {
		t.Fatalf("atomic create/mark args = %v, want %v", got, want)
	}
}

func TestNewManagedDetachedArgsMarksOnlyAfterSuccessfulCreateInSameQueue(t *testing.T) {
	got := NewManagedDetachedArgs("agent", "/bin/bash", 1234)
	want := []string{
		"new-session", "-d", "-s", "agent", "/bin/bash",
		";", "set-option", "-q", "-t", "agent", managedSessionOption, managedSessionOptionValue,
		";", "set-option", "-q", "-t", "agent", managedSessionLeaseOption, "1234",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NewManagedDetachedArgs() = %v, want %v", got, want)
	}
}

func TestManager_EnsureSessionCreateRaceNeverAdoptsUnmarkedWinner(t *testing.T) {
	f := newFakeRunner()
	displays := 0
	f.run = func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			displays++
			if displays == 1 {
				return nil, exitError(t)
			}
			return []byte("agent\t$2\t42\t0\t\t0\n"), nil
		case "new-session":
			return []byte("duplicate session: agent"), errors.New("create lost race")
		default:
			return nil, fmt.Errorf("unexpected command: %v", args)
		}
	}
	m := NewManagerWith("tmux", f)
	err := m.EnsureSessionLease(context.Background(), "agent", "/bin/bash", time.Now().Add(time.Hour))
	if !errors.Is(err, ErrSessionNameCollision) {
		t.Fatalf("error = %v, want collision", err)
	}
	for _, call := range f.calls {
		if call[0] == "set-option" || call[0] == "kill-session" {
			t.Fatalf("race winner received a follow-up mutation: calls=%v", f.calls)
		}
	}
	if len(f.calls) != 3 || f.calls[1][0] != "new-session" || !containsArg(f.calls[1], managedSessionOption) {
		t.Fatalf("ownership was not confined to the failed creator queue: calls=%v", f.calls)
	}
}

func TestManager_EnsureSessionUsesScopeCommand(t *testing.T) {
	f := newFakeRunner()
	f.errs["display-message"] = exitError(t)
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
	got := f.calls[len(f.calls)-2]
	wantPrefix := []string{"--scope", "--", "/usr/bin/tmux", "new-session", "-d", "-s", "agent", "/bin/bash"}
	if len(got) < len(wantPrefix) || !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) || !containsArg(got, managedSessionOption) || !containsArg(got, managedSessionLeaseOption) {
		t.Fatalf("scoped create/mark args = %v", got)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

type reaperRunner struct {
	calls        [][]string
	now          time.Time
	sessions     map[string]SessionStatus
	beforeAtomic func(map[string]SessionStatus)
}

func (r *reaperRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "list-sessions":
		var rows []string
		for _, name := range []string{"expired", "became-attached", "renewed", "recent", "attached", "operator", "malformed"} {
			status, ok := r.sessions[name]
			if !ok {
				continue
			}
			marker := ""
			if status.Managed {
				marker = managedSessionOptionValue
			}
			attached := 0
			if status.Attached {
				attached = 1
			}
			lease := strconv.FormatInt(status.LeaseExpires, 10)
			if name == "malformed" {
				lease = "not-a-lease"
			}
			rows = append(rows, fmt.Sprintf("%s\t$%d\t42\t%d\t%s\t%s", name, len(rows)+1, attached, marker, lease))
		}
		return []byte(strings.Join(rows, "\n") + "\n"), nil
	case "if-shell":
		if r.beforeAtomic != nil {
			r.beforeAtomic(r.sessions)
			r.beforeAtomic = nil
		}
		name := args[3]
		status, ok := r.sessions[name]
		if ok && status.Managed && !status.Attached && strings.Contains(args[4], fmt.Sprintf(",%d}", status.LeaseExpires)) {
			delete(r.sessions, name)
		}
		return nil, nil
	case "has-session":
		if _, ok := r.sessions[args[2]]; !ok {
			return nil, exitErrorForRunner()
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command: %v", args)
	}
}

func exitErrorForRunner() error {
	cmd := exec.Command("false")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit 1")
	}
	return cmd.Run()
}

func TestManager_ReapExpiredSessionsOnlyKillsMatchingLeaseAtomically(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	runner := &reaperRunner{now: now, sessions: map[string]SessionStatus{
		"expired":         {Name: "expired", ID: "$1", ServerPID: 42, Managed: true, LeaseExpires: now.Add(-time.Hour).Unix()},
		"became-attached": {Name: "became-attached", ID: "$2", ServerPID: 42, Managed: true, LeaseExpires: now.Add(-time.Hour).Unix()},
		"renewed":         {Name: "renewed", ID: "$3", ServerPID: 42, Managed: true, LeaseExpires: now.Add(-time.Hour).Unix()},
		"recent":          {Name: "recent", ID: "$4", ServerPID: 42, Managed: true, LeaseExpires: now.Add(time.Hour).Unix()},
		"attached":        {Name: "attached", ID: "$5", ServerPID: 42, Attached: true, Managed: true, LeaseExpires: now.Add(-time.Hour).Unix()},
		"operator":        {Name: "operator", ID: "$6", ServerPID: 42, LeaseExpires: now.Add(-time.Hour).Unix()},
		"malformed":       {Name: "malformed", ID: "$7", ServerPID: 42, Managed: true},
	}}
	runner.beforeAtomic = func(sessions map[string]SessionStatus) {
		status := sessions["became-attached"]
		status.Attached = true
		sessions["became-attached"] = status
		status = sessions["renewed"]
		status.LeaseExpires = now.Add(time.Hour).Unix()
		sessions["renewed"] = status
	}
	manager := NewManagerWith("tmux", runner)

	reaped, err := manager.ReapExpiredSessions(context.Background(), now)
	if err != nil {
		t.Fatalf("ReapExpiredSessions() error: %v", err)
	}
	if want := []string{"expired"}; !reflect.DeepEqual(reaped, want) {
		t.Fatalf("reaped = %v, want %v", reaped, want)
	}

	var conditional []string
	for _, call := range runner.calls {
		if len(call) >= 4 && call[0] == "if-shell" {
			conditional = append(conditional, call[3])
		}
	}
	if want := []string{"expired", "became-attached", "renewed"}; !reflect.DeepEqual(conditional, want) {
		t.Fatalf("conditional targets = %v, want %v; calls=%v", conditional, want, runner.calls)
	}
}

func TestParseSessionStatusesFailsClosed(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	out := fmt.Sprintf("managed\t$1\t42\t0\t%s\t%d\noperator\t$2\t42\t0\t\t%d\nbad name\t$3\t42\t0\t%s\t%d\nbad-attached\t$4\t42\tnope\t%s\t%d\n", managedSessionOptionValue, now.Unix(), now.Unix(), managedSessionOptionValue, now.Unix(), managedSessionOptionValue, now.Unix())
	got := parseSessionStatuses([]byte(out))
	want := []SessionStatus{
		{Name: "managed", ID: "$1", ServerPID: 42, Managed: true, LeaseExpires: now.Unix()},
		{Name: "operator", ID: "$2", ServerPID: 42, LeaseExpires: now.Unix()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSessionStatuses() = %#v, want %#v", got, want)
	}
}

func TestReapExpiredLeaseArgsIncludesAtomicGuards(t *testing.T) {
	args := ReapExpiredLeaseArgs("agent", 1234)
	joined := strings.Join(args, " ")
	for _, want := range []string{"if-shell", "#{session_attached}", managedSessionOption, managedSessionOptionValue, managedSessionLeaseOption, "1234", "kill-session -t agent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(sessionStatusFormat, "session_activity") {
		t.Fatalf("reaper must not infer task idleness from session_activity: %q", sessionStatusFormat)
	}
}

func TestManager_ReapExpiredSessionsHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFakeRunner()
	f.run = func(ctx context.Context, _ []string) ([]byte, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m := NewManagerWith("tmux", f)
	_, err := m.ReapExpiredSessions(ctx, time.Now())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
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
