// cmd/citadel-start/main_test.go
package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSiblingPath(t *testing.T) {
	// The child must be resolved next to the launcher, never via PATH/CWD.
	dir := filepath.FromSlash("/opt/citadel")
	self := filepath.Join(dir, "citadel-start.exe")
	got := siblingPath(self, childName)
	want := filepath.Join(dir, childName)
	if got != want {
		t.Fatalf("siblingPath(%q, %q) = %q, want %q", self, childName, got, want)
	}
}

func TestExitCodeFromErr(t *testing.T) {
	if got := exitCodeFromErr(nil); got != 0 {
		t.Fatalf("nil err: got %d, want 0", got)
	}

	// Non-ExitError (e.g. binary could not be launched) -> 1.
	if got := exitCodeFromErr(errors.New("boom")); got != 1 {
		t.Fatalf("generic err: got %d, want 1", got)
	}

	// A real *exec.ExitError should surface the child's own code.
	// `exit 3` via the platform shell gives us a deterministic non-zero code.
	var shell, flag string
	if runtime.GOOS == "windows" {
		shell, flag = "cmd", "/c"
	} else {
		shell, flag = "sh", "-c"
	}
	err := exec.Command(shell, flag, "exit 3").Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T (%v)", err, err)
	}
	if got := exitCodeFromErr(err); got != 3 {
		t.Fatalf("exit-3 child: got %d, want 3", got)
	}
}

func TestShouldPause(t *testing.T) {
	unset := func(string) string { return "" }
	if !shouldPause(unset) {
		t.Fatal("expected pause when env var unset")
	}

	set := func(k string) string {
		if k == noPauseEnv {
			return "1"
		}
		return ""
	}
	if shouldPause(set) {
		t.Fatalf("expected no pause when %s is set", noPauseEnv)
	}
}

func TestExitHint(t *testing.T) {
	if got := exitHint(0); got != "" {
		t.Fatalf("clean exit should produce no hint, got %q", got)
	}

	hint := exitHint(1)
	if hint == "" {
		t.Fatal("non-zero exit should produce a hint")
	}
	// The hint must guide the user toward initialization, the dominant
	// first-run failure mode.
	for _, want := range []string{"code 1", childName + " init"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint %q missing %q", hint, want)
		}
	}
}

func TestRunMissingChild(t *testing.T) {
	// When citadel.exe is absent next to the launcher, run must not exec
	// anything, must explain the problem, and must return code 2.
	self := filepath.Join(t.TempDir(), "citadel-start.exe")
	statMissing := func(string) (os.FileInfo, error) {
		return nil, fs.ErrNotExist
	}
	execCalled := false
	execImpl := func(string) int {
		execCalled = true
		return 0
	}

	var out bytes.Buffer
	code := run(self, statMissing, execImpl, &out)

	if code != 2 {
		t.Fatalf("missing child: got code %d, want 2", code)
	}
	if execCalled {
		t.Fatal("must not exec the child when it is missing")
	}
	if !strings.Contains(out.String(), childName) {
		t.Fatalf("output should name the missing binary, got %q", out.String())
	}
}

func TestRunForwardsExitCode(t *testing.T) {
	// A present child whose "work" run fails must have its exit code
	// forwarded, and the uninitialized-node hint shown.
	self := filepath.Join(t.TempDir(), "citadel-start.exe")
	statOK := func(string) (os.FileInfo, error) {
		return fakeFileInfo{}, nil
	}
	var gotChildPath string
	execImpl := func(childPath string) int {
		gotChildPath = childPath
		return 1
	}

	var out bytes.Buffer
	code := run(self, statOK, execImpl, &out)

	if code != 1 {
		t.Fatalf("got code %d, want 1 (forwarded)", code)
	}
	if want := siblingPath(self, childName); gotChildPath != want {
		t.Fatalf("exec'd %q, want sibling %q", gotChildPath, want)
	}
	if !strings.Contains(out.String(), childName+" init") {
		t.Fatalf("failed run should show init hint, got %q", out.String())
	}
}

func TestRunCleanExit(t *testing.T) {
	self := filepath.Join(t.TempDir(), "citadel-start.exe")
	statOK := func(string) (os.FileInfo, error) { return fakeFileInfo{}, nil }
	execImpl := func(string) int { return 0 }

	var out bytes.Buffer
	code := run(self, statOK, execImpl, &out)

	if code != 0 {
		t.Fatalf("got code %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Citadel stopped") {
		t.Fatalf("clean exit should report a stop, got %q", out.String())
	}
	if strings.Contains(out.String(), "init") {
		t.Fatalf("clean exit should not show the init hint, got %q", out.String())
	}
}

func TestPauseNoOpWhenDisabled(t *testing.T) {
	// With the pause disabled, pause must not consume stdin or print a prompt.
	disabled := func(k string) string {
		if k == noPauseEnv {
			return "1"
		}
		return ""
	}
	in := strings.NewReader("should-not-be-read\n")
	var out bytes.Buffer
	pause(disabled, &out, in)

	if out.Len() != 0 {
		t.Fatalf("disabled pause should print nothing, got %q", out.String())
	}
	rest, _ := in.ReadByte()
	if rest != 's' {
		t.Fatalf("disabled pause consumed stdin (next byte %q), want untouched", rest)
	}
}

func TestPauseWaitsForEnter(t *testing.T) {
	enabled := func(string) string { return "" }
	in := strings.NewReader("\n")
	var out bytes.Buffer
	pause(enabled, &out, in)

	if !strings.Contains(out.String(), "Press Enter") {
		t.Fatalf("enabled pause should prompt, got %q", out.String())
	}
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return childName }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }
