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

func memoryGitFixtureEngine(t *testing.T) *memorygit.Engine {
	t.Helper()
	engine, err := memorygit.New("node-fixture", t.TempDir(), filepath.Join(t.TempDir(), "git"), map[string]string{"person-fixture": "memories/person-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func memoryGitFixtureState() (PasscodeState, error) {
	return PasscodeState{Set: true, Check: func(pin string) PasscodeVerdict {
		if pin == "synthetic-window" {
			return PasscodeMatch
		}
		return PasscodeMismatch
	}}, nil
}

func TestNewMemoryGitHandlerRequiresStrictDependencies(t *testing.T) {
	engine := memoryGitFixtureEngine(t)
	calls := 0
	enabled := func() bool { calls++; return true }
	state := func() (PasscodeState, error) {
		calls++
		return memoryGitFixtureState()
	}
	throttle := NewPasscodeThrottle()
	for _, missing := range []string{"engine", "permission", "state", "throttle"} {
		t.Run(missing, func(t *testing.T) {
			e, permission, load, budget := engine, enabled, state, throttle
			switch missing {
			case "engine":
				e = nil
			case "permission":
				permission = nil
			case "state":
				load = nil
			case "throttle":
				budget = nil
			}
			h, err := NewMemoryGitHandler(e, permission, load, budget)
			var typed *memorygit.Error
			if h != nil || !errors.As(err, &typed) || typed.Code != "invalid_configuration" || calls != 0 {
				t.Fatal("constructor accepted missing wiring or evaluated live callbacks")
			}
		})
	}
	h, err := NewMemoryGitHandler(engine, enabled, state, throttle)
	if err != nil || h == nil || calls != 0 {
		t.Fatal("valid construction evaluated live callbacks or refused")
	}
	for _, incomplete := range []*MemoryGitHandler{
		nil, {},
		{enabled: enabled, state: state, throttle: throttle},
		{engine: engine, state: state, throttle: throttle},
		{engine: engine, enabled: enabled, throttle: throttle},
		{engine: engine, enabled: enabled, state: state},
	} {
		_, err := incomplete.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, "synthetic-window"))
		var typed *memorygit.Error
		if !errors.As(err, &typed) || typed.Code != "invalid_configuration" || calls != 0 {
			t.Fatal("nil/zero/incomplete handler bypassed constructor policy")
		}
	}
}

func TestMemoryGitShellGateParity(t *testing.T) {
	for _, test := range []struct {
		name      string
		enabled   bool
		state     func() (PasscodeState, error)
		pin, want string
	}{
		{"disabled", false, memoryGitFixtureState, "synthetic-window", ReasonShellDisabled},
		{"missing passcode", true, func() (PasscodeState, error) { return PasscodeState{}, nil }, "", ReasonPasscodeNotSet},
		{"missing verifier", true, func() (PasscodeState, error) { return PasscodeState{Set: true}, nil }, "synthetic-window", ReasonPasscodeInvalid},
		{"wrong credential", true, memoryGitFixtureState, "wrong", ReasonPasscodeInvalid},
		{"unavailable state", true, func() (PasscodeState, error) {
			return PasscodeState{}, errors.New("private fixture error")
		}, "synthetic-window", ReasonPasscodeUnavailable},
		{"vault lockout", true, func() (PasscodeState, error) {
			return PasscodeState{Set: true, Check: func(string) PasscodeVerdict { return PasscodeLockedOut }}, nil
		}, "synthetic-window", ReasonPasscodeLocked},
		{"unavailable check", true, func() (PasscodeState, error) {
			return PasscodeState{Set: true, Check: func(string) PasscodeVerdict { return PasscodeUnavailable }}, nil
		}, "synthetic-window", ReasonPasscodeUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			enabled := func() bool { return test.enabled }
			h, err := NewMemoryGitHandler(memoryGitFixtureEngine(t), enabled, test.state, NewPasscodeThrottle())
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, test.pin))
			var refusal *ShellRefusal
			if !errors.As(err, &refusal) || refusal.Reason != test.want {
				t.Fatalf("got %v, want %s", err, test.want)
			}
			gate := ShellGate{Enabled: enabled, State: test.state, Throttle: NewPasscodeThrottle()}
			if expected := ShellGateReason(gate, test.pin); expected != refusal.Reason {
				t.Fatalf("shell gate parity: %s != %s", expected, refusal.Reason)
			}
			if strings.Contains(err.Error(), test.pin) && test.pin != "" {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestMemoryGitSharesThrottleAndLiveRevocation(t *testing.T) {
	for _, first := range []string{"shell", "memory"} {
		t.Run(first, func(t *testing.T) {
			enabled := true
			stateCalls, checks := 0, 0
			state := func() (PasscodeState, error) {
				stateCalls++
				return PasscodeState{Set: true, Check: func(pin string) PasscodeVerdict {
					checks++
					if pin == "synthetic-window" {
						return PasscodeMatch
					}
					return PasscodeMismatch
				}}, nil
			}
			throttle := NewPasscodeThrottle()
			throttle.MaxFailures = 1
			live := func() bool { return enabled }
			h, err := NewMemoryGitHandler(memoryGitFixtureEngine(t), live, state, throttle)
			if err != nil {
				t.Fatal(err)
			}
			shell := &ShellCommandHandler{Enabled: live, State: state, Throttle: throttle}
			ctx := JobContext{LogFn: func(string, string) {}}
			run := func(surface, pin string) error {
				if surface == "shell" {
					// No command field: even a gate regression cannot start a shell.
					_, err := shell.Execute(ctx, &nexus.Job{ID: "fixture-shell", Type: "SHELL_COMMAND", Payload: map[string]string{ShellPasscodePayloadKey: pin}})
					return err
				}
				_, err := h.Execute(ctx, memoryGitJob(memoryGitFixtureRequest, pin))
				return err
			}
			var refusal *ShellRefusal
			if err := run(first, "wrong"); !errors.As(err, &refusal) || refusal.Reason != ReasonPasscodeInvalid || checks != 1 {
				t.Fatal("wrong credential was not counted through the actual first handler")
			}
			other := "memory"
			if first == "memory" {
				other = "shell"
			}
			if err := run(other, "synthetic-window"); !errors.As(err, &refusal) || refusal.Reason != ReasonPasscodeThrottled || checks != 1 {
				t.Fatal("actual handlers did not share one failure budget")
			}
			before := stateCalls
			enabled = false
			if err := run("memory", "synthetic-window"); !errors.As(err, &refusal) || refusal.Reason != ReasonShellDisabled || stateCalls != before || checks != 1 {
				t.Fatal("live revoke lost precedence or invoked strict state/check")
			}
		})
	}
}

func TestMemoryGitStrictPayload(t *testing.T) {
	h, err := NewMemoryGitHandler(memoryGitFixtureEngine(t), func() bool { return true }, memoryGitFixtureState, NewPasscodeThrottle())
	if err != nil {
		t.Fatal(err)
	}
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
	for _, key := range []string{"command", "target_node", "timeout_ms", "rayId"} {
		job := memoryGitJob(memoryGitFixtureRequest, "synthetic-window")
		job.Payload[key] = "ignored"
		_, err := h.Execute(JobContext{}, job)
		var typed *memorygit.Error
		if !errors.As(err, &typed) || typed.Code != "invalid_request" {
			t.Fatal("extra top-level operand was not refused specifically before repository access")
		}
	}
	if _, err := h.Execute(JobContext{}, nil); err == nil {
		t.Fatal("nil job accepted")
	}
}

func TestMemoryGitStrictStateFailurePrecedesSharedThrottle(t *testing.T) {
	throttle := NewPasscodeThrottle()
	throttle.MaxFailures = 1
	enabled, unavailable := true, false
	state := func() (PasscodeState, error) {
		if unavailable {
			return PasscodeState{}, errors.New("private-state-read-detail")
		}
		return memoryGitFixtureState()
	}
	h, err := NewMemoryGitHandler(memoryGitFixtureEngine(t), func() bool { return enabled }, state, throttle)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, "wrong"))
	var refusal *ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != ReasonPasscodeInvalid || !throttle.Throttled() {
		t.Fatal("fixture did not consume the actual shared throttle")
	}
	unavailable = true
	_, err = h.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, "synthetic-window"))
	if !errors.As(err, &refusal) || refusal.Reason != ReasonPasscodeUnavailable || strings.Contains(err.Error(), "private-state-read-detail") {
		t.Fatal("strict state read failure was hidden by throttling or disclosed raw details")
	}
	enabled = false
	_, err = h.Execute(JobContext{}, memoryGitJob(memoryGitFixtureRequest, "synthetic-window"))
	if !errors.As(err, &refusal) || refusal.Reason != ReasonShellDisabled {
		t.Fatal("disabled permission lost precedence over unavailable strict state")
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
		// Keep maintenance from racing the fixture metadata-permission walk.
		argv := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0", "-C", repo}, args...)
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
	h, err := NewMemoryGitHandler(engine, func() bool { return true }, func() (PasscodeState, error) {
		return PasscodeState{Set: true, Check: func(pin string) PasscodeVerdict {
			checked = pin
			if pin == "synthetic-window" {
				return PasscodeMatch
			}
			return PasscodeMismatch
		}}, nil
	}, NewPasscodeThrottle())
	if err != nil {
		t.Fatal(err)
	}
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
