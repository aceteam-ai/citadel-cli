package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func controlJob(id string) *Job {
	return &Job{
		ID: id, Type: JobTypeWorkerControl, Source: "redis",
		SourceQueue: "jobs:v1:shell:org_example:node:758",
		Payload:     map[string]any{"action": "restart", "target_node": "758", "timeout_ms": "25000"},
	}
}

type controlSource struct {
	*MockJobSource
	events *[]string
	ackErr error
}

func (s *controlSource) Ack(ctx context.Context, job *Job) error {
	*s.events = append(*s.events, "ack")
	if s.ackErr != nil {
		return s.ackErr
	}
	return s.MockJobSource.Ack(ctx, job)
}

type controlWriter struct {
	NoOpStreamWriter
	events *[]string
	result map[string]any
	err    error
}

func (w *controlWriter) WriteEnd(result map[string]any) error {
	*w.events = append(*w.events, "result")
	w.result = result
	return w.err
}

func runControl(t *testing.T, h *WorkerControlHandler, job *Job, publishErr, ackErr error, sequence *[]string) ([]string, map[string]any, bool) {
	t.Helper()
	if sequence == nil {
		sequence = &[]string{}
	}
	source := &controlSource{MockJobSource: NewMockJobSource("redis", nil), events: sequence, ackErr: ackErr}
	writer := &controlWriter{events: sequence, err: publishErr}
	runner := NewRunner(source, []JobHandler{h}, RunnerConfig{NodeID: "758", State: NewWorkerState()})
	runner.WithStreamWriterFactory(func(*Job) StreamWriter { return writer })
	oldAttempts, oldBackoff := streamWriteRetryAttempts, streamWriteRetryBackoff
	streamWriteRetryAttempts, streamWriteRetryBackoff = 1, []time.Duration{0}
	defer func() { streamWriteRetryAttempts, streamWriteRetryBackoff = oldAttempts, oldBackoff }()
	proceed, stream, start := runner.claimJob(context.Background(), job)
	if !proceed {
		return *sequence, writer.result, false
	}
	ok := runner.executeJob(context.Background(), job, stream, start, false, 0)
	return *sequence, writer.result, ok
}

func TestWorkerControlPublishesAndAcksBeforeRestart(t *testing.T) {
	events := []string{}
	h := NewWorkerControlHandler(WorkerControlConfig{
		NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true },
		Restart: func() error { events = append(events, "restart"); return nil },
	})
	job := controlJob("restart-1")
	// The writer and source record their own events; the restart callback is
	// asserted through its observable marker and count below.
	got, result, ok := runControl(t, h, job, nil, nil, &events)
	if !ok || result["accepted"] != true || result["restarting"] != true {
		t.Fatalf("result=%v ok=%v", result, ok)
	}
	if !reflect.DeepEqual(got, []string{"result", "ack", "restart"}) {
		t.Fatalf("publish/ack=%v restart=%v", got, events)
	}
	if fired, err := h.marker(job.ID, "fired"); err != nil {
		t.Fatal(err)
	} else if duplicate, err := createDurableMarker(fired); err != nil || !duplicate {
		t.Fatal("fired marker missing")
	}
}

func TestWorkerControlRejectsMalformedControls(t *testing.T) {
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Restart: func() error { t.Fatal("unexpected restart"); return nil }})
	cases := []struct {
		name string
		edit func(*Job)
	}{
		{"unknown action", func(j *Job) { j.Payload["action"] = "shutdown" }},
		{"missing action", func(j *Job) { delete(j.Payload, "action") }},
		{"wrong target", func(j *Job) { j.Payload["target_node"] = "759" }},
		{"malformed target", func(j *Job) { j.Payload["target_node"] = 758 }},
		{"wrong queue", func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_example:node:759" }},
		{"shared queue", func(j *Job) { j.SourceQueue = "jobs:v1:shell:org_example" }},
		{"non redis", func(j *Job) { j.Source = "nexus" }},
		{"unknown field", func(j *Job) { j.Payload["command"] = "reboot" }},
		{"malformed timeout", func(j *Job) { j.Payload["timeout_ms"] = []string{"1"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := controlJob(tc.name)
			tc.edit(job)
			_, result, ok := runControl(t, h, job, nil, nil, nil)
			if !ok || result["accepted"] != false || result["restarting"] != false || result["code"] == "" {
				t.Fatalf("result=%v ok=%v", result, ok)
			}
		})
	}
}

func TestWorkerControlDuplicateDoesNotRestartAgain(t *testing.T) {
	count := 0
	cfg := WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Restart: func() error { count++; return nil }}
	job := controlJob("same-job")
	for i := 0; i < 3; i++ {
		// Constructing a fresh handler models a process restart between deliveries.
		_, result, ok := runControl(t, NewWorkerControlHandler(cfg), job, nil, nil, nil)
		if !ok || result["accepted"] != true {
			t.Fatalf("delivery %d: result=%v ok=%v", i, result, ok)
		}
	}
	if count != 1 {
		t.Fatalf("restart count=%d, want 1", count)
	}
}

func TestWorkerControlNewJobIsCoalescedDuringCooldown(t *testing.T) {
	count := 0
	cfg := WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Restart: func() error { count++; return nil }}
	h := NewWorkerControlHandler(cfg)
	_, first, ok := runControl(t, h, controlJob("first"), nil, nil, nil)
	if !ok || first["accepted"] != true {
		t.Fatalf("first result=%v ok=%v", first, ok)
	}
	_, second, ok := runControl(t, NewWorkerControlHandler(cfg), controlJob("second"), nil, nil, nil)
	if !ok || second["accepted"] != false || second["code"] != "restart_cooldown" || count != 1 {
		t.Fatalf("second result=%v ok=%v restart count=%d", second, ok, count)
	}
}

func TestWorkerControlDoesNotRestartWithoutResultAndAck(t *testing.T) {
	for _, tc := range []struct {
		name               string
		publishErr, ackErr error
	}{
		{"publish failure", errors.New("publish unavailable"), nil},
		{"ack failure", nil, errors.New("ack unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Restart: func() error { count++; return nil }})
			_, _, ok := runControl(t, h, controlJob("retry-me"), tc.publishErr, tc.ackErr, nil)
			if ok || count != 0 {
				t.Fatalf("ok=%v restart count=%d", ok, count)
			}
			_, result, ok := runControl(t, h, controlJob("retry-me"), nil, nil, nil)
			if !ok || result["accepted"] != true || count != 1 {
				t.Fatalf("retry: ok=%v result=%v restart count=%d", ok, result, count)
			}
		})
	}
}

func TestWorkerControlRestartFailureDoesNotLoop(t *testing.T) {
	count := 0
	cfg := WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Restart: func() error { count++; return errors.New("service manager unavailable") }}
	job := controlJob("failed-restart")
	for i := 0; i < 2; i++ {
		_, result, ok := runControl(t, NewWorkerControlHandler(cfg), job, nil, nil, nil)
		if !ok || result["accepted"] != true {
			t.Fatalf("delivery %d: ok=%v result=%v", i, ok, result)
		}
	}
	if count != 1 {
		t.Fatalf("failed restart attempted %d times, want 1", count)
	}
}
