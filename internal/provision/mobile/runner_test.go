package mobile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile creates an empty file at path, used by builder tests for existence
// checks.
func writeFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}

func TestRunner_DryRunPrintsAndDoesNotExecute(t *testing.T) {
	var buf bytes.Buffer
	r := NewRunner(true, &buf)
	executed := false
	r.execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		executed = true
		return nil, nil
	}
	copied := false
	r.copyFile = func(src, dst string) error {
		copied = true
		return nil
	}

	steps := []Step{
		{Kind: StepExec, Desc: "exec step", Name: "security", Args: []string{"create-keychain", "-p", "pw", "kc"}, SecretArgs: []int{2}},
		{Kind: StepCopyFile, Desc: "copy step", SrcPath: "/a/x.mobileprovision", DstPath: "/b/x.mobileprovision"},
	}
	if err := r.Run(steps); err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if executed {
		t.Error("exec hook called during dry run")
	}
	if copied {
		t.Error("copy hook called during dry run")
	}

	out := buf.String()
	if !strings.Contains(out, "security create-keychain -p <redacted> kc") {
		t.Errorf("dry-run exec line missing/unredacted:\n%s", out)
	}
	if strings.Contains(out, "pw") {
		t.Errorf("password leaked in dry-run output:\n%s", out)
	}
	if !strings.Contains(out, "copy /a/x.mobileprovision -> /b/x.mobileprovision") {
		t.Errorf("dry-run copy line missing:\n%s", out)
	}
}

func TestRunner_RealRunInvokesHooks(t *testing.T) {
	var buf bytes.Buffer
	r := NewRunner(false, &buf)

	var gotName string
	var gotArgs []string
	r.execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = args
		return []byte("ok\n"), nil
	}
	var gotSrc, gotDst string
	r.copyFile = func(src, dst string) error {
		gotSrc, gotDst = src, dst
		return nil
	}

	steps := []Step{
		{Kind: StepExec, Desc: "run", Name: "sdkmanager", Args: []string{"--licenses"}},
		{Kind: StepCopyFile, Desc: "install", SrcPath: "/a/p.mobileprovision", DstPath: "/b/p.mobileprovision"},
	}
	if err := r.Run(steps); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if gotName != "sdkmanager" || len(gotArgs) != 1 || gotArgs[0] != "--licenses" {
		t.Errorf("exec got %q %v", gotName, gotArgs)
	}
	if gotSrc != "/a/p.mobileprovision" || gotDst != "/b/p.mobileprovision" {
		t.Errorf("copy got %q -> %q", gotSrc, gotDst)
	}
	if !strings.Contains(buf.String(), "ok") {
		t.Errorf("command output not echoed: %s", buf.String())
	}
}

func TestRunner_StopsOnFirstError(t *testing.T) {
	r := NewRunner(false, &bytes.Buffer{})
	calls := 0
	r.execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		return nil, errors.New("boom")
	}
	steps := []Step{
		{Kind: StepExec, Desc: "first", Name: "a"},
		{Kind: StepExec, Desc: "second", Name: "b"},
	}
	err := r.Run(steps)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "step 1") || !strings.Contains(err.Error(), "first") {
		t.Errorf("error should name failing step: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected to stop after 1 exec, got %d", calls)
	}
}

// fakeExitError is a test fake satisfying the exitCoder interface.
type fakeExitError struct {
	code int
	msg  string
}

func (e *fakeExitError) Error() string { return e.msg }
func (e *fakeExitError) ExitCode() int { return e.code }

func TestRunner_ToleratesAllowedExitCode(t *testing.T) {
	var buf bytes.Buffer
	r := NewRunner(false, &buf)
	r.execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("keychain already exists\n"), &fakeExitError{code: 48, msg: "exit status 48"}
	}

	steps := []Step{
		{
			Kind:             StepExec,
			Desc:             "Create keychain (tolerate 48)",
			Name:             "security",
			Args:             []string{"create-keychain", "-p", "pw", "kc"},
			AllowedExitCodes: []int{48},
		},
	}
	if err := r.Run(steps); err != nil {
		t.Fatalf("expected success when exit code is allowed, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "exit code 48 tolerated") {
		t.Errorf("expected tolerance note in output, got:\n%s", out)
	}
}

func TestRunner_RejectsUnallowedExitCode(t *testing.T) {
	r := NewRunner(false, &bytes.Buffer{})
	r.execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, &fakeExitError{code: 1, msg: "exit status 1"}
	}

	steps := []Step{
		{
			Kind:             StepExec,
			Desc:             "Create keychain (only 48 allowed)",
			Name:             "security",
			Args:             []string{"create-keychain", "-p", "pw", "kc"},
			AllowedExitCodes: []int{48},
		},
	}
	err := r.Run(steps)
	if err == nil {
		t.Fatal("expected error for non-allowed exit code, got nil")
	}
	if !strings.Contains(err.Error(), "security failed") {
		t.Errorf("error = %q, want substring %q", err, "security failed")
	}
}

func TestRunner_NoAllowedExitCodesStillFails(t *testing.T) {
	r := NewRunner(false, &bytes.Buffer{})
	r.execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, &fakeExitError{code: 48, msg: "exit status 48"}
	}

	steps := []Step{
		{
			Kind: StepExec,
			Desc: "No allowed codes set",
			Name: "security",
			Args: []string{"create-keychain", "-p", "pw", "kc"},
			// AllowedExitCodes intentionally empty
		},
	}
	err := r.Run(steps)
	if err == nil {
		t.Fatal("expected error when AllowedExitCodes is empty, got nil")
	}
}

func TestDefaultCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mobileprovision")
	if err := os.WriteFile(src, []byte("PROFILE-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "nested", "dst.mobileprovision")

	if err := defaultCopyFile(src, dst); err != nil {
		t.Fatalf("copy failed: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if string(got) != "PROFILE-BYTES" {
		t.Errorf("dst content = %q, want PROFILE-BYTES", got)
	}
}
