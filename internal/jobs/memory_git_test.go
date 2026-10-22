package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/memorygit"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func memoryGitJob(raw, pin string) *nexus.Job {
	return &nexus.Job{ID: "fixture-job", Type: memorygit.JobType, Payload: map[string]string{"request": raw, ShellPasscodePayloadKey: pin}}
}

const memoryGitFixtureRequest = `{"version":1,"node_id":"node-fixture","scope":"person-fixture","operation":"resolve","revision":"HEAD"}`

func TestMemoryGitShellGateParity(t *testing.T) {
	for _, test := range []struct {
		name      string
		gate      ShellGate
		pin, want string
	}{
		{"missing live permission", ShellGate{}, "", ReasonShellDisabled},
		{"disabled", ShellGate{Enabled: func() bool { return false }}, "synthetic-window", ReasonShellDisabled},
		{"missing passcode", ShellGate{Enabled: func() bool { return true }}, "", ReasonPasscodeNotSet},
		{"missing verifier", ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return true }}, "synthetic-window", ReasonPasscodeInvalid},
		{"wrong credential", ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return true }, VerifyPasscode: func(string) bool { return false }}, "wrong", ReasonPasscodeInvalid},
		{"unavailable state", ShellGate{Enabled: func() bool { return true }, State: func() (PasscodeState, error) { return PasscodeState{}, errors.New("private fixture error") }}, "synthetic-window", ReasonPasscodeUnavailable},
		{"vault lockout", ShellGate{Enabled: func() bool { return true }, State: func() (PasscodeState, error) {
			return PasscodeState{Set: true, Check: func(string) PasscodeVerdict { return PasscodeLockedOut }}, nil
		}}, "synthetic-window", ReasonPasscodeLocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := MemoryGitHandler{Gate: test.gate}
			_, err := h.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, test.pin))
			var refusal *ShellRefusal
			if !errors.As(err, &refusal) || refusal.Reason != test.want {
				t.Fatalf("got %v, want %s", err, test.want)
			}
			if test.gate.Enabled != nil {
				if expected := ShellGateReason(test.gate, test.pin); expected != refusal.Reason {
					t.Fatalf("shell gate parity: %s != %s", expected, refusal.Reason)
				}
			}
			if strings.Contains(err.Error(), test.pin) && test.pin != "" {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestMemoryGitSharesThrottleAndLiveRevocation(t *testing.T) {
	enabled := true
	throttle := NewPasscodeThrottle()
	throttle.MaxFailures = 1
	gate := ShellGate{Enabled: func() bool { return enabled }, HasPasscode: func() bool { return true }, VerifyPasscode: func(pin string) bool { return pin == "synthetic-window" }, Throttle: throttle}
	// A wrong attempt on the existing shell gate consumes the shared budget.
	if got := ShellGateReason(gate, "wrong"); got != ReasonPasscodeInvalid {
		t.Fatal(got)
	}
	h := MemoryGitHandler{Gate: gate}
	_, err := h.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, "synthetic-window"))
	var refusal *ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != ReasonPasscodeThrottled {
		t.Fatalf("shared throttle: %v", err)
	}
	enabled = false
	_, err = h.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, "synthetic-window"))
	if !errors.As(err, &refusal) || refusal.Reason != ReasonShellDisabled {
		t.Fatalf("live revoke: %v", err)
	}
}

func TestMemoryGitStrictPayload(t *testing.T) {
	engine, err := memorygit.New("node-fixture", t.TempDir(), filepath.Join(t.TempDir(), "git"), map[string]string{"person-fixture": "memories/person-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	h := MemoryGitHandler{Engine: engine, Gate: ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return true }, VerifyPasscode: func(pin string) bool { return pin == "synthetic-window" }}}
	for _, raw := range []string{
		"", strings.Repeat("x", memorygit.MaxRequestBytes+1), "null", "[]", memoryGitFixtureRequest + " {}",
		strings.Replace(memoryGitFixtureRequest, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(memoryGitFixtureRequest, `"version":1`, `"Version":1`, 1),
		strings.Replace(memoryGitFixtureRequest, `"version":1`, `"version":null`, 1),
		strings.Replace(memoryGitFixtureRequest, `"version":1`, `"version":"1"`, 1),
		strings.Replace(memoryGitFixtureRequest, `"revision":"HEAD"`, `"revision":"HEAD","command":"ignored"`, 1),
	} {
		_, err := h.Execute(JobContext{}, memoryGitJob(raw, "synthetic-window"))
		var typed *memorygit.Error
		if !errors.As(err, &typed) || typed.Code != "invalid_request" {
			t.Fatalf("payload was not refused before repo access: %v", err)
		}
	}
	job := memoryGitJob(memoryGitFixtureRequest, "synthetic-window")
	job.Payload["command"] = "ignored"
	if _, err := h.Execute(JobContext{}, job); err == nil {
		t.Fatal("extra top-level operand accepted")
	}
	if _, err := h.Execute(JobContext{}, nil); err == nil {
		t.Fatal("nil job accepted")
	}
}

func TestMemoryGitAuthorizedExportAndContext(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor runner is Linux only")
	}
	repo := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.Abs(git)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		argv := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-C", repo}, args...)
		cmd := exec.Command(git, argv...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + repo, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture git: %v %s", err, out)
		}
	}
	run("init", "--quiet")
	if err := os.MkdirAll(filepath.Join(repo, "memories/person-fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "memories/person-fixture/_note.md"), []byte("fixture content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "fixture")
	if err := filepath.WalkDir(filepath.Join(repo, ".git"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0022
		if path == filepath.Join(repo, ".git") {
			mode = 0700
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	engine, err := memorygit.New("node-fixture", repo, git, map[string]string{"person-fixture": "memories/person-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	checked := ""
	h := MemoryGitHandler{Engine: engine, Gate: ShellGate{Enabled: func() bool { return true }, State: func() (PasscodeState, error) {
		return PasscodeState{Set: true, Check: func(pin string) PasscodeVerdict {
			checked = pin
			if pin == "synthetic-window" {
				return PasscodeMatch
			}
			return PasscodeMismatch
		}}, nil
	}}}
	raw := `{"version":1,"node_id":"node-fixture","scope":"person-fixture","operation":"export","name":"_note","revision":"HEAD"}`
	job := memoryGitJob(raw, "synthetic-window")
	output, err := h.Execute(JobContext{}, job)
	if err != nil {
		t.Fatal(err)
	}
	var result memorygit.Result
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	content, err := base64.StdEncoding.DecodeString(result.ContentBase64)
	if err != nil || string(content) != "fixture content\n" || checked != "synthetic-window" || strings.Contains(string(output), checked) {
		t.Fatalf("bad authorized export: %s %v", output, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Execute(JobContext{Ctx: ctx}, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("context was not propagated: %v", err)
	}
}
