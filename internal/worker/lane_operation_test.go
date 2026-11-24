package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/laneop"
)

const (
	laneQueue    = "jobs:v1:shell:org_org:node:42"
	laneSentinel = "SENTINEL-lane-send-41d2"
	lanePasscode = "1357-2468"
	laneRule     = "────────────────────────────────────────"
)

// laneFakeTmux serves one pane with an empty input box and records every call.
type laneFakeTmux struct {
	calls [][]string
	draft string
	sent  int
}

func (f *laneFakeTmux) exec(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch args[0] {
	case "-V":
		return []byte("tmux 3.4\n"), nil
	case "list-panes":
		return []byte("%7\n"), nil
	case "display-message":
		return []byte(args[3] + "|$1|0|0|0|0|0\n"), nil
	case "capture-pane":
		return []byte("output\n" + laneRule + "\n> " + f.draft + "\n" + laneRule + "\n"), nil
	case "list-clients":
		return nil, nil
	case "load-buffer":
		f.draft = string(stdin)
		return nil, nil
	case "paste-buffer", "delete-buffer":
		return nil, nil
	case "send-keys":
		f.sent++
		f.draft = ""
		return nil, nil
	}
	return nil, errors.New("unexpected")
}

type laneEnv struct {
	shell     bool
	set       bool
	available bool
}

func newLaneHandler(t *testing.T, env *laneEnv, tmux *laneFakeTmux) *LaneOperationHandler {
	t.Helper()
	return NewLaneOperationHandler(LaneOperationConfig{
		OrgID: "org", NodeID: "42",
		Gate: jobs.ShellGate{
			Enabled: func() bool { return env.shell },
			State: func() (jobs.PasscodeState, error) {
				return jobs.PasscodeState{Set: env.set, Check: func(pin string) jobs.PasscodeVerdict {
					if pin == lanePasscode {
						return jobs.PasscodeMatch
					}
					return jobs.PasscodeMismatch
				}}, nil
			},
			Throttle: jobs.NewPasscodeThrottle(),
		},
		Service: &laneop.Service{
			Exec: tmux.exec, Ledger: laneop.NewLedger(t.TempDir()),
			Sleep: func(context.Context, time.Duration) error { return nil },
		},
		Available: func() bool { return env.available },
	})
}

func laneJob(payload map[string]any) *Job {
	base := map[string]any{"lane": "lane-a", "lease_id": "lease-1", "node_id": "42", "pane": "%7", "target_node": "42", "passcode": lanePasscode}
	for k, v := range payload {
		if v == nil {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	return &Job{ID: "job-1", Type: JobTypeLaneOperation, Source: "redis-api", SourceQueue: laneQueue, Payload: base}
}

func capturePayload() map[string]any {
	return map[string]any{"operation": "capture", "max_bytes": "4096"}
}
func sendPayload() map[string]any {
	return map[string]any{"operation": "send", "message": laneSentinel, "offer_id": "offer-1"}
}

func refusalOf(t *testing.T, err error) string {
	t.Helper()
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want a terminal ShellRefusal, got %v", err)
	}
	return refusal.Reason
}

func TestLaneOperationCaptureAndSend(t *testing.T) {
	env := &laneEnv{shell: true, set: true, available: true}
	tmux := &laneFakeTmux{}
	h := newLaneHandler(t, env, tmux)
	if !h.CanHandle(JobTypeLaneOperation) || h.CanHandle(JobTypeShellCommand) {
		t.Fatal("advertisement")
	}
	r, err := h.Execute(context.Background(), laneJob(capturePayload()), nil)
	if err != nil || r.Status != JobStatusSuccess || r.Output["busy_evidence"] != "prompt_visible" || r.Output["pane_busy"] != false {
		t.Fatalf("capture %+v %v", r, err)
	}
	r, err = h.Execute(context.Background(), laneJob(sendPayload()), nil)
	if err != nil || r.Output["status"] != "submitted" || tmux.sent != 1 {
		t.Fatalf("send %+v %v sent=%d", r, err, tmux.sent)
	}
	// Redelivery of the same job: no second Enter.
	r, _ = h.Execute(context.Background(), laneJob(sendPayload()), nil)
	if r.Output["status"] != "submitted" || r.Output["replayed"] != true || tmux.sent != 1 {
		t.Fatalf("redelivery %+v sent=%d", r.Output, tmux.sent)
	}
	out, _ := json.Marshal(r.Output)
	if strings.Contains(string(out), laneSentinel) {
		t.Fatal("message echoed in the result")
	}
}

func TestLaneOperationRefusesStreamAndNodeMismatch(t *testing.T) {
	cases := map[string]func(*Job){
		"shared org stream":     func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_org" },
		"another node's stream": func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_org:node:43" },
		"another org":           func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_other:node:42" },
		"websocket label":       func(j *Job) { j.Source = "ws" },
		"payload node":          func(j *Job) { j.Payload["node_id"] = "43" },
		"missing payload node":  func(j *Job) { delete(j.Payload, "node_id") },
		"target node":           func(j *Job) { j.Payload["target_node"] = "43" },
	}
	for name, mutate := range cases {
		for _, op := range []map[string]any{capturePayload(), sendPayload()} {
			t.Run(fmt.Sprint(name, "/", op["operation"]), func(t *testing.T) {
				env := &laneEnv{shell: true, set: true, available: true}
				tmux := &laneFakeTmux{}
				h := newLaneHandler(t, env, tmux)
				throttle := h.cfg.Gate.Throttle
				job := laneJob(op)
				job.Payload["passcode"] = "wrong"
				mutate(job)
				_, err := h.Execute(context.Background(), job, nil)
				if reason := refusalOf(t, err); reason != "stream_mismatch" && reason != "node_mismatch" {
					t.Fatalf("reason %s", reason)
				}
				if len(tmux.calls) != 0 {
					t.Fatalf("tmux touched: %v", tmux.calls)
				}
				// A misrouted job never counts against the passcode throttle.
				for i := 0; i < jobs.DefaultPasscodeMaxFailures+1; i++ {
					h.Execute(context.Background(), job, nil)
				}
				if throttle.Throttled() {
					t.Fatal("misrouted jobs throttled the node passcode")
				}
			})
		}
	}
}

func TestLaneOperationGatesRefuse(t *testing.T) {
	cases := []struct {
		name   string
		env    laneEnv
		pass   any
		reason string
	}{
		{"shell disabled", laneEnv{shell: false, set: true, available: true}, lanePasscode, jobs.ReasonShellDisabled},
		{"no passcode set", laneEnv{shell: true, set: false, available: true}, nil, jobs.ReasonPasscodeNotSet},
		{"wrong passcode", laneEnv{shell: true, set: true, available: true}, "0000", jobs.ReasonPasscodeInvalid},
		{"missing passcode", laneEnv{shell: true, set: true, available: true}, nil, jobs.ReasonPasscodeInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			tmux := &laneFakeTmux{}
			h := newLaneHandler(t, &env, tmux)
			capture := capturePayload()
			capture["passcode"] = tc.pass
			_, err := h.Execute(context.Background(), laneJob(capture), nil)
			if reason := refusalOf(t, err); reason != tc.reason {
				t.Fatalf("capture reason %s", reason)
			}
			send := sendPayload()
			send["passcode"] = tc.pass
			r, err := h.Execute(context.Background(), laneJob(send), nil)
			if err != nil || r.Status != JobStatusSuccess || r.Output["status"] != "rejected" || r.Output["reason"] != tc.reason {
				t.Fatalf("send %+v %v", r, err)
			}
			if len(tmux.calls) != 0 {
				t.Fatalf("tmux touched past the gate: %v", tmux.calls)
			}
		})
	}
}

func TestLaneOperationStrictPayload(t *testing.T) {
	cases := map[string]map[string]any{
		"extra key":           {"operation": "capture", "max_bytes": "10", "cmd": "rm"},
		"bound too large":     {"operation": "capture", "max_bytes": "8193"},
		"bound not canonical": {"operation": "capture", "max_bytes": "010"},
		"bad pane":            {"operation": "capture", "max_bytes": "10", "pane": "lane-a:0.0"},
		"bad lane":            {"operation": "capture", "max_bytes": "10", "lane": "Lane A"},
		"unknown operation":   {"operation": "control", "max_bytes": "10"},
		"send control bytes":  {"operation": "send", "message": "x\x1b[2J", "offer_id": "o"},
		"send bad offer":      {"operation": "send", "message": "x", "offer_id": "a b"},
		"send with bound":     {"operation": "send", "message": "x", "offer_id": "o", "max_bytes": "10"},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			tmux := &laneFakeTmux{}
			h := newLaneHandler(t, &laneEnv{shell: true, set: true, available: true}, tmux)
			r, err := h.Execute(context.Background(), laneJob(payload), nil)
			if payload["operation"] == "send" {
				if err != nil || r.Output["status"] != "rejected" {
					t.Fatalf("send %+v %v", r, err)
				}
			} else {
				refusalOf(t, err)
			}
			if len(tmux.calls) != 0 {
				t.Fatalf("tmux touched: %v", tmux.calls)
			}
		})
	}
}

func TestLaneOperationNotAdvertisedWithoutTmuxOrWiring(t *testing.T) {
	h := newLaneHandler(t, &laneEnv{shell: true, set: true, available: false}, &laneFakeTmux{})
	if h.CanHandle(JobTypeLaneOperation) {
		t.Fatal("advertised without tmux")
	}
	if got := h.RefusalFor(JobTypeLaneOperation); got == nil || got.Reason != "capability_unavailable" {
		t.Fatalf("refusal %+v", got)
	}
	if _, err := h.Execute(context.Background(), laneJob(capturePayload()), nil); refusalOf(t, err) != "capability_unavailable" {
		t.Fatal("executed without tmux")
	}
	disabled := newLaneHandler(t, &laneEnv{shell: false, set: true, available: true}, &laneFakeTmux{})
	if disabled.CanHandle(JobTypeLaneOperation) || disabled.RefusalFor(JobTypeLaneOperation).Reason != jobs.ReasonShellDisabled {
		t.Fatal("advertised with shell disabled")
	}
	for name, cfg := range map[string]LaneOperationConfig{
		"zero":        {},
		"no throttle": {OrgID: "org", NodeID: "42", Gate: jobs.ShellGate{Enabled: func() bool { return true }, State: func() (jobs.PasscodeState, error) { return jobs.PasscodeState{}, nil }}, Service: &laneop.Service{Exec: (&laneFakeTmux{}).exec, Ledger: laneop.NewLedger(t.TempDir())}, Available: func() bool { return true }},
		"no ledger":   {OrgID: "org", NodeID: "42", Gate: jobs.ShellGate{Enabled: func() bool { return true }, State: func() (jobs.PasscodeState, error) { return jobs.PasscodeState{}, nil }, Throttle: jobs.NewPasscodeThrottle()}, Service: &laneop.Service{Exec: (&laneFakeTmux{}).exec}, Available: func() bool { return true }},
	} {
		if NewLaneOperationHandler(cfg).CanHandle(JobTypeLaneOperation) {
			t.Fatalf("%s: advertised", name)
		}
	}
	if h.RefusalFor(JobTypeShellCommand) != nil {
		t.Fatal("answered for another type")
	}
}

func TestLaneOperationMessageNeverRetained(t *testing.T) {
	job := laneJob(sendPayload())
	job.RawPayload = []byte(`{"message":"` + laneSentinel + `"}`)
	rendered, _ := json.Marshal(job)
	for name, text := range map[string]string{
		"json":      string(rendered),
		"fmt":       fmt.Sprintf("%v %+v", *job, *job),
		"dlq":       mustJSON(t, jobprivacy.Payload(job.Type, job.Payload)),
		"reason":    jobprivacy.Text(job.Type, job.Payload, "failed while typing "+laneSentinel),
		"failure":   mustJSON(t, jobprivacy.FailureData(job.Type, job.Payload, errors.New("x "+laneSentinel), nil)),
		"other key": mustJSON(t, jobprivacy.Payload(job.Type, job.Payload)),
	} {
		if strings.Contains(text, laneSentinel) || strings.Contains(text, lanePasscode) {
			t.Fatalf("%s retains the message: %s", name, text)
		}
	}
	if got := jobprivacy.Payload(job.Type, job.Payload); got["offer_id"] != "offer-1" {
		t.Fatalf("dropped more than the message: %v", got)
	}
	// Another job type keeps its own message field.
	other := map[string]any{"message": "kept"}
	if got := jobprivacy.Payload(JobTypeInstanceMessage, other); got["message"] != "kept" {
		t.Fatal("message dropped for another job type")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
