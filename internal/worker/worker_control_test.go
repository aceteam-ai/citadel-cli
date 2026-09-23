package worker

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

func controlJob(id string) *Job {
	return &Job{ID: id, Type: JobTypeWorkerControl, Source: "redis", SourceQueue: "jobs:v1:shell:org_example:node:758",
		Payload: map[string]any{"action": "restart", "target_node": "758", "timeout_ms": "25000"}}
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
	if w.err == nil {
		w.result = result
	}
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
func testSchedule(afterCommit func()) func() (func(), func(), error) {
	return func() (func(), func(), error) { return afterCommit, func() {}, nil }
}

func TestWorkerControlAcksAndPublishesBeforeActualRestart(t *testing.T) {
	events := []string{}
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { events = append(events, "restart") })})
	job := controlJob("restart-1")
	got, result, ok := runControl(t, h, job, nil, nil, &events)
	if !ok || result["accepted"] != true || result["restarting"] != true || !reflect.DeepEqual(got, []string{"ack", "result", "restart"}) {
		t.Fatalf("events=%v result=%v ok=%v", got, result, ok)
	}
	if fired, err := h.marker(job.ID, "fired"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(fired); err != nil {
		t.Fatal("fired marker missing:", err)
	}
}

func TestWorkerControlRejectsMalformedControls(t *testing.T) {
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { t.Fatal("unexpected restart") })})
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
		{"nonnumeric timeout", func(j *Job) { j.Payload["timeout_ms"] = "soon" }},
		{"zero timeout", func(j *Job) { j.Payload["timeout_ms"] = "0" }},
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

func TestWorkerControlDuplicateAndCooldownNeverScheduleTwice(t *testing.T) {
	count := 0
	cfg := WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { count++ })}
	for i, id := range []string{"same-job", "same-job", "other-job"} {
		_, result, ok := runControl(t, NewWorkerControlHandler(cfg), controlJob(id), nil, nil, nil)
		if !ok {
			t.Fatalf("job %s failed: %v", id, result)
		}
		if i == 0 && result["accepted"] != true {
			t.Fatalf("first result=%v", result)
		}
		if i == 1 && (result["accepted"] != false || result["code"] != "duplicate_control") {
			t.Fatalf("duplicate result=%v", result)
		}
		if i == 2 && (result["accepted"] != false || result["code"] != "restart_cooldown") {
			t.Fatalf("new job during cooldown=%v", result)
		}
	}
	if count != 1 {
		t.Fatalf("restart count=%d, want 1", count)
	}
}

func TestWorkerControlAckFailureNeverPublishesAcceptance(t *testing.T) {
	count := 0
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { count++ })})
	job := controlJob("ack-failed")
	events, result, ok := runControl(t, h, job, nil, errors.New("ack unavailable"), nil)
	if ok || result["accepted"] != false || result["restarting"] != false || result["code"] != "ack_failed" || count != 0 || !reflect.DeepEqual(events, []string{"ack", "result"}) {
		t.Fatalf("events=%v result=%v ok=%v count=%d", events, result, ok, count)
	}
	_, duplicate, _ := runControl(t, h, job, nil, nil, nil)
	if duplicate["accepted"] != false || duplicate["code"] != "aborted_control" {
		t.Fatalf("redelivery=%v", duplicate)
	}
}

func TestWorkerControlSchedulingFailureDoesNotAcceptOrTombstone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule func() (func(), func(), error)
	}{
		{"scheduler error", func() (func(), func(), error) { return nil, nil, errors.New("scheduler unavailable") }},
		{"missing commit callback", func() (func(), func(), error) { return nil, func() {}, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: stateDir, Managed: func() bool { return true }, Schedule: tc.schedule})
			job := controlJob("schedule-failed")
			events, result, ok := runControl(t, h, job, nil, nil, nil)
			if ok || result["accepted"] != false || result["restarting"] != false || result["code"] != "schedule_failed" || !reflect.DeepEqual(events, []string{"ack", "result"}) {
				t.Fatalf("events=%v result=%v ok=%v", events, result, ok)
			}
			if fired, err := h.marker(job.ID, "fired"); err != nil {
				t.Fatal(err)
			} else if _, err := os.Stat(fired); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fired marker survived schedule failure: %v", err)
			}
			good := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: stateDir, Managed: func() bool { return true }, Schedule: testSchedule(func() {})})
			_, next, ok := runControl(t, good, controlJob("new-job"), nil, nil, nil)
			if !ok || next["accepted"] != true {
				t.Fatalf("fresh retry result=%v ok=%v", next, ok)
			}
		})
	}
}

func TestWorkerControlPublishFailureCancelsPreparedRestart(t *testing.T) {
	count, cancelled := 0, 0
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func() (func(), func(), error) { return func() { count++ }, func() { cancelled++ }, nil }})
	job := controlJob("publish-failed")
	_, result, ok := runControl(t, h, job, errors.New("publish unavailable"), nil, nil)
	if ok || result != nil || count != 0 || cancelled != 1 {
		t.Fatalf("result=%v ok=%v restarts=%d cancelled=%d", result, ok, count, cancelled)
	}
	if fired, err := h.marker(job.ID, "fired"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(fired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fired marker survived publish failure: %v", err)
	}
}

func TestWorkerControlSchedulerErrorCancelsPreparedWork(t *testing.T) {
	cancelled := 0
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true },
		Schedule: func() (func(), func(), error) {
			return nil, func() { cancelled++ }, errors.New("prepare failed")
		}})
	_, result, ok := runControl(t, h, controlJob("partial-prepare"), nil, nil, nil)
	if ok || result["accepted"] != false || result["code"] != "schedule_failed" || cancelled != 1 {
		t.Fatalf("result=%v ok=%v cancelled=%d", result, ok, cancelled)
	}
}

func TestWorkerControlBlockedAccountingCannotDelayAcceptedRestart(t *testing.T) {
	restarted := make(chan struct{})
	recording := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true },
		Schedule: testSchedule(func() { close(restarted) })})
	events := []string{}
	source := &controlSource{MockJobSource: NewMockJobSource("redis", nil), events: &events}
	writer := &controlWriter{events: &events}
	runner := NewRunner(source, []JobHandler{h}, RunnerConfig{NodeID: "758", State: NewWorkerState(),
		JobRecordFn: func(usage.UsageRecord) { close(recording); <-release }})
	done := make(chan bool, 1)
	go func() {
		done <- runner.executeJob(context.Background(), controlJob("blocked-recorder"), writer, time.Now(), false, 0)
	}()
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("accepted result did not release restart gate before accounting blocked")
	}
	select {
	case <-recording:
	case <-time.After(2 * time.Second):
		t.Fatal("usage recorder was not called")
	}
	if writer.result["accepted"] != true || !reflect.DeepEqual(events, []string{"ack", "result"}) {
		t.Fatalf("events=%v result=%v", events, writer.result)
	}
	close(release)
	released = true
	if !<-done {
		t.Fatal("accepted control did not finish")
	}
}

func TestWorkerControlPanickingAccountingCannotPreventAcceptedRestart(t *testing.T) {
	restarted := false
	h := NewWorkerControlHandler(WorkerControlConfig{NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true },
		Schedule: testSchedule(func() { restarted = true })})
	events := []string{}
	source := &controlSource{MockJobSource: NewMockJobSource("redis", nil), events: &events}
	writer := &controlWriter{events: &events}
	runner := NewRunner(source, []JobHandler{h}, RunnerConfig{NodeID: "758", State: NewWorkerState(),
		JobRecordFn: func(usage.UsageRecord) { panic("usage recorder failed") }})
	ok := runner.executeJob(context.Background(), controlJob("panic-recorder"), writer, time.Now(), false, 0)
	if !ok || !restarted || writer.result["accepted"] != true || !reflect.DeepEqual(events, []string{"ack", "result"}) {
		t.Fatalf("restart gate=%v events=%v result=%v", restarted, events, writer.result)
	}
}
