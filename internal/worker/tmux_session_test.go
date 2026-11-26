package worker

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

const (
	tmuxQueue    = "jobs:v1:shell:org_org:node:42"
	tmuxPasscode = "2468-1357"
	tmuxShell    = "SENTINEL-session-shell-7c1e"
)

// fakeTmuxSessions is the exec seam: it records every job that reaches tmux.
type fakeTmuxSessions struct{ jobs []*nexus.Job }

func (f *fakeTmuxSessions) Execute(_ jobs.JobContext, job *nexus.Job) ([]byte, error) {
	f.jobs = append(f.jobs, job)
	return []byte(`{"action":"ensure"}`), nil
}

type tmuxEnv struct {
	shell    bool
	set      bool
	throttle *jobs.PasscodeThrottle
}

func newTmuxHandler(env *tmuxEnv, sessions *fakeTmuxSessions) *TmuxSessionHandler {
	if env.throttle == nil {
		env.throttle = jobs.NewPasscodeThrottle()
	}
	return NewTmuxSessionHandler(TmuxSessionConfig{
		OrgID: "org", NodeID: "42",
		Gate: jobs.ShellGate{
			Enabled: func() bool { return env.shell },
			State: func() (jobs.PasscodeState, error) {
				return jobs.PasscodeState{Set: env.set, Check: func(pin string) jobs.PasscodeVerdict {
					if pin == tmuxPasscode {
						return jobs.PasscodeMatch
					}
					return jobs.PasscodeMismatch
				}}, nil
			},
			Throttle: env.throttle,
		},
		Sessions: sessions,
	})
}

func tmuxJob(payload map[string]any) *Job {
	base := map[string]any{"action": "ensure", "name": "agent", "shell": tmuxShell, "node_id": "42", "target_node": "42", "passcode": tmuxPasscode}
	for k, v := range payload {
		if v == nil {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	return &Job{ID: "job-1", Type: JobTypeTmuxSession, Source: "redis-api", SourceQueue: tmuxQueue, Payload: base}
}

func TestTmuxSessionEnsureRunsWithGate(t *testing.T) {
	sessions := &fakeTmuxSessions{}
	h := newTmuxHandler(&tmuxEnv{shell: true, set: true}, sessions)
	result, err := h.Execute(context.Background(), tmuxJob(nil), nil)
	if err != nil || result.Status != JobStatusSuccess {
		t.Fatalf("ensure = %+v, %v", result, err)
	}
	if len(sessions.jobs) != 1 {
		t.Fatalf("tmux calls = %d, want 1", len(sessions.jobs))
	}
	got := sessions.jobs[0].Payload
	if got["shell"] != tmuxShell || got["name"] != "agent" {
		t.Fatalf("tmux payload = %v", got)
	}
	if _, ok := got[jobs.ShellPasscodePayloadKey]; ok {
		t.Fatal("the passcode reached tmux")
	}
	if result.Output["output"] != `{"action":"ensure"}` {
		t.Fatalf("output = %v", result.Output)
	}
}

func TestTmuxSessionRefusesStreamAndNodeMismatch(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Job)
		reason string
	}{
		"shared org stream":   {func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_org" }, "stream_mismatch"},
		"other node stream":   {func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_org:node:43" }, "stream_mismatch"},
		"other org stream":    {func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_other:node:42" }, "stream_mismatch"},
		"non-redis source":    {func(j *Job) { j.Source = "nexus" }, "stream_mismatch"},
		"wrong node_id":       {func(j *Job) { j.Payload["node_id"] = "43" }, "node_mismatch"},
		"missing node_id":     {func(j *Job) { delete(j.Payload, "node_id") }, "node_mismatch"},
		"numeric node_id":     {func(j *Job) { j.Payload["node_id"] = 42.0 }, "node_mismatch"},
		"wrong target_node":   {func(j *Job) { j.Payload["target_node"] = "43" }, "node_mismatch"},
		"list on shared pool": {func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_org"; j.Payload["action"] = "list" }, "stream_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			env := &tmuxEnv{shell: true, set: true}
			sessions := &fakeTmuxSessions{}
			h := newTmuxHandler(env, sessions)
			job := tmuxJob(map[string]any{"passcode": "wrong"})
			tc.mutate(job)
			_, err := h.Execute(context.Background(), job, nil)
			if got := refusalOf(t, err); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
			if len(sessions.jobs) != 0 {
				t.Fatal("a refused job reached tmux")
			}
			if env.throttle.Throttled() {
				t.Fatal("a misrouted job counted toward the passcode throttle")
			}
		})
	}
}

func TestTmuxSessionShellGateRefuses(t *testing.T) {
	limited := func() *jobs.PasscodeThrottle {
		return &jobs.PasscodeThrottle{MaxFailures: 1, Window: time.Minute, Cooldown: time.Minute}
	}
	for name, tc := range map[string]struct {
		env     tmuxEnv
		payload map[string]any
		reason  string
	}{
		"shell off":             {tmuxEnv{shell: false, set: true}, nil, jobs.ReasonShellDisabled},
		"no passcode set":       {tmuxEnv{shell: true, set: false}, nil, jobs.ReasonPasscodeNotSet},
		"missing passcode":      {tmuxEnv{shell: true, set: true}, map[string]any{"passcode": nil}, jobs.ReasonPasscodeInvalid},
		"wrong passcode":        {tmuxEnv{shell: true, set: true}, map[string]any{"passcode": "wrong"}, jobs.ReasonPasscodeInvalid},
		"create alias":          {tmuxEnv{shell: true, set: true}, map[string]any{"action": "create", "passcode": "wrong"}, jobs.ReasonPasscodeInvalid},
		"empty action":          {tmuxEnv{shell: true, set: true}, map[string]any{"action": nil, "passcode": "wrong"}, jobs.ReasonPasscodeInvalid},
		"blank action":          {tmuxEnv{shell: true, set: true}, map[string]any{"action": "  ", "passcode": "wrong"}, jobs.ReasonPasscodeInvalid},
		"mixed-case action":     {tmuxEnv{shell: true, set: true}, map[string]any{"action": " Ensure ", "passcode": "wrong"}, jobs.ReasonPasscodeInvalid},
		"unknown action":        {tmuxEnv{shell: true, set: true}, map[string]any{"action": "kill", "passcode": "wrong"}, jobs.ReasonPasscodeInvalid},
		"unknown with passcode": {tmuxEnv{shell: true, set: true}, map[string]any{"action": "kill"}, "invalid_request"},
		"non-string value":      {tmuxEnv{shell: true, set: true}, map[string]any{"shell": 7.0}, "invalid_request"},
		"list with shell off":   {tmuxEnv{shell: false, set: true}, map[string]any{"action": "list"}, jobs.ReasonShellDisabled},
		"has with shell off":    {tmuxEnv{shell: false, set: true}, map[string]any{"action": "has"}, jobs.ReasonShellDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			env := tc.env
			sessions := &fakeTmuxSessions{}
			h := newTmuxHandler(&env, sessions)
			_, err := h.Execute(context.Background(), tmuxJob(tc.payload), nil)
			if got := refusalOf(t, err); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
			if len(sessions.jobs) != 0 {
				t.Fatal("a refused job reached tmux")
			}
		})
	}

	t.Run("throttled", func(t *testing.T) {
		env := &tmuxEnv{shell: true, set: true, throttle: limited()}
		sessions := &fakeTmuxSessions{}
		h := newTmuxHandler(env, sessions)
		if _, err := h.Execute(context.Background(), tmuxJob(map[string]any{"passcode": "wrong"}), nil); refusalOf(t, err) != jobs.ReasonPasscodeInvalid {
			t.Fatalf("first wrong passcode: %v", err)
		}
		_, err := h.Execute(context.Background(), tmuxJob(nil), nil)
		if got := refusalOf(t, err); got != jobs.ReasonPasscodeThrottled {
			t.Fatalf("reason = %q, want %q", got, jobs.ReasonPasscodeThrottled)
		}
		if len(sessions.jobs) != 0 {
			t.Fatal("a throttled job reached tmux")
		}
	})
}

func TestTmuxSessionListAndHasNeedOnlyOwnStream(t *testing.T) {
	for _, action := range []string{"list", "has", " LIST "} {
		sessions := &fakeTmuxSessions{}
		h := newTmuxHandler(&tmuxEnv{shell: true, set: false}, sessions)
		if _, err := h.Execute(context.Background(), tmuxJob(map[string]any{"action": action, "passcode": nil}), nil); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if len(sessions.jobs) != 1 {
			t.Fatalf("%s: tmux calls = %d, want 1", action, len(sessions.jobs))
		}
	}
}

func TestTmuxSessionAdvertisementFollowsLiveGate(t *testing.T) {
	env := &tmuxEnv{shell: true, set: true}
	h := newTmuxHandler(env, &fakeTmuxSessions{})
	runner := NewRunner(nil, []JobHandler{h}, RunnerConfig{})
	if !slices.Contains(runner.SupportedJobTypes(), JobTypeTmuxSession) || h.CanHandle(JobTypeShellCommand) {
		t.Fatal("not advertised with shell on")
	}
	env.shell = false
	if slices.Contains(runner.SupportedJobTypes(), JobTypeTmuxSession) {
		t.Fatal("advertised with shell off")
	}
	if got := runner.unavailableJobRefusal(JobTypeTmuxSession); got == nil || got.Reason != jobs.ReasonShellDisabled {
		t.Fatalf("refusal = %+v, want shell_disabled", got)
	}
	env.shell = true
	if !slices.Contains(runner.SupportedJobTypes(), JobTypeTmuxSession) {
		t.Fatal("not re-advertised when shell came back on")
	}
}

func TestTmuxSessionNotAdvertisedWithoutWiring(t *testing.T) {
	gate := jobs.ShellGate{Enabled: func() bool { return true }, State: func() (jobs.PasscodeState, error) { return jobs.PasscodeState{}, nil }, Throttle: jobs.NewPasscodeThrottle()}
	for name, cfg := range map[string]TmuxSessionConfig{
		"no org":      {NodeID: "42", Gate: gate, Sessions: &fakeTmuxSessions{}},
		"no node":     {OrgID: "org", Gate: gate, Sessions: &fakeTmuxSessions{}},
		"bad org":     {OrgID: "org:node:9", NodeID: "42", Gate: gate, Sessions: &fakeTmuxSessions{}},
		"no state":    {OrgID: "org", NodeID: "42", Gate: jobs.ShellGate{Enabled: gate.Enabled, Throttle: gate.Throttle}, Sessions: &fakeTmuxSessions{}},
		"no throttle": {OrgID: "org", NodeID: "42", Gate: jobs.ShellGate{Enabled: gate.Enabled, State: gate.State}, Sessions: &fakeTmuxSessions{}},
		"no enabled":  {OrgID: "org", NodeID: "42", Gate: jobs.ShellGate{State: gate.State, Throttle: gate.Throttle}, Sessions: &fakeTmuxSessions{}},
		"no sessions": {OrgID: "org", NodeID: "42", Gate: gate},
	} {
		h := NewTmuxSessionHandler(cfg)
		if h.CanHandle(JobTypeTmuxSession) {
			t.Errorf("%s: advertised", name)
		}
		if got := h.RefusalFor(JobTypeTmuxSession); got == nil || got.Reason != "capability_unavailable" {
			t.Errorf("%s: refusal = %+v", name, got)
		}
		if _, err := h.Execute(context.Background(), tmuxJob(nil), nil); refusalOf(t, err) != "capability_unavailable" {
			t.Errorf("%s: executed", name)
		}
	}
}

func TestTmuxSessionNotRegisteredAsLegacyAdapter(t *testing.T) {
	for _, h := range postureHandlers(t) {
		if h.CanHandle(JobTypeTmuxSession) {
			t.Fatal("the legacy handler set still answers TMUX_SESSION")
		}
	}
}
