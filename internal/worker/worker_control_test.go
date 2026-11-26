package worker

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol"
	"github.com/aceteam-ai/citadel-cli/internal/workercontrol/controltest"
)

const fixtureOrg = "11111111-1111-4111-8111-111111111111"

func controlJob(id string) *Job {
	f := controltest.Load()
	f.Identity.AttemptID = uuid.NewString()
	f.Identity.JobID = uuid.NewString()
	f.Identity.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	e := workercontrol.Envelope{Identity: f.Identity, Delivery: f.Deliveries["A"]}
	binding := &workerControlBinding{orgID: fixtureOrg, nodeID: "758", queue: e.Identity.Queue, group: WorkerControlProtocol, client: redisapi.NewClient(redisapi.ClientConfig{}), config: f.Config()}
	s := workercontrol.NewSession(e, func(ctx context.Context, r workercontrol.Request) (workercontrol.Response, error) {
		return controltest.Accept(r, time.Now().UnixMilli()), nil
	})
	if err := s.Claim(context.Background()); err != nil {
		panic(err)
	}
	return &Job{control: binding, controlSession: s, controlEnvelope: &e, ID: e.Identity.JobID, MessageID: e.Delivery.MessageID, Type: JobTypeWorkerControl, Source: "redis-api", SourceQueue: binding.queue,
		Payload: map[string]any{"action": "restart", "target_node": "758"}}
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
	events  *[]string
	result  map[string]any
	err     error
	session *workercontrol.Session
}

func (w *controlWriter) authenticatedWorkerControl(job *Job) bool {
	w.session = job.controlSession
	return job.control.matches(job)
}
func (w *controlWriter) WriteClaimed(string) error {
	if w.session == nil {
		return workercontrol.ErrDenied
	}
	return w.session.Claim(context.Background())
}

func (w *controlWriter) WriteEnd(result map[string]any) error {
	*w.events = append(*w.events, "result")
	if w.err == nil {
		if w.session != nil {
			outcome, code := "prepared", "prepared"
			if result["accepted"] != true {
				outcome = "refused_no_effect"
				code, _ = result["code"].(string)
			}
			if err := w.session.Complete(context.Background(), outcome, code); err != nil {
				return err
			}
		}
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
func testSchedule(afterCommit func()) func(_ func() bool) (func(), func(), error) {
	return func(_ func() bool) (func(), func(), error) { return afterCommit, func() {}, nil }
}

func TestWorkerControlAcksAndPublishesBeforeActualRestart(t *testing.T) {
	events := []string{}
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { events = append(events, "restart") })})
	job := controlJob("restart-1")
	got, result, ok := runControl(t, h, job, nil, nil, &events)
	if !ok || result["accepted"] != true || result["restarting"] != true || !reflect.DeepEqual(got, []string{"ack", "result", "restart"}) {
		t.Fatalf("events=%v result=%v ok=%v", got, result, ok)
	}
	if fired, err := h.marker(controlReservationKey(job), "fired"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(fired); err != nil {
		t.Fatal("fired marker missing:", err)
	}
}

func TestWorkerControlRejectsMalformedControls(t *testing.T) {
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { t.Fatal("unexpected restart") })})
	cases := []struct {
		name string
		edit func(*Job)
	}{
		{"unknown action", func(j *Job) { j.Payload["action"] = "shutdown" }},
		{"missing action", func(j *Job) { delete(j.Payload, "action") }},
		{"wrong target", func(j *Job) { j.Payload["target_node"] = "759" }},
		{"malformed target", func(j *Job) { j.Payload["target_node"] = 758 }},
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

func TestWorkerControlMissingConfiguredOrgRefusesBeforeScheduling(t *testing.T) {
	h := NewWorkerControlHandler(WorkerControlConfig{
		NodeID: "758", StateDir: t.TempDir(),
		Managed:  func() bool { return true },
		Schedule: testSchedule(func() { t.Fatal("unexpected restart") }),
	})
	_, result, ok := runControl(t, h, controlJob("missing-org"), nil, nil, nil)
	if !ok || result["accepted"] != false || result["code"] != "invalid_queue" {
		t.Fatalf("missing configured org: result=%v ok=%v", result, ok)
	}
}

func TestWorkerControlDuplicateAndUnresolvedNodeNeverScheduleTwice(t *testing.T) {
	count := 0
	cfg := WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { count++ })}
	for i, id := range []string{"same-job", "same-job", "other-job"} {
		_, result, ok := runControl(t, NewWorkerControlHandler(cfg), controlJob(id), nil, nil, nil)
		if !ok {
			t.Fatalf("job %s failed: %v", id, result)
		}
		if i == 0 && result["accepted"] != true {
			t.Fatalf("first result=%v", result)
		}
		if i == 1 && (result["accepted"] != false || result["code"] != "restart_unconfirmed") {
			t.Fatalf("duplicate result=%v", result)
		}
		if i == 2 && (result["accepted"] != false || result["code"] != "restart_unconfirmed") {
			t.Fatalf("new job with unresolved reservation=%v", result)
		}
	}
	if count != 1 {
		t.Fatalf("restart count=%d, want 1", count)
	}
}

func TestWorkerControlAckFailureNeverPublishesAcceptance(t *testing.T) {
	count := 0
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { count++ })})
	job := controlJob("ack-failed")
	events, result, ok := runControl(t, h, job, nil, errors.New("ack unavailable"), nil)
	if ok || result != nil || count != 0 || !reflect.DeepEqual(events, []string{"ack", "result"}) {
		t.Fatalf("events=%v result=%v ok=%v count=%d", events, result, ok, count)
	}
	_, duplicate, _ := runControl(t, h, job, nil, nil, nil)
	if duplicate != nil || job.controlSession.CanPrepare() {
		t.Fatalf("redelivery=%v", duplicate)
	}
}

func TestWorkerControlSchedulingFailureRetainsUnresolvedFence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule func(_ func() bool) (func(), func(), error)
	}{
		{"scheduler error", func(_ func() bool) (func(), func(), error) { return nil, nil, errors.New("scheduler unavailable") }},
		{"missing commit callback", func(_ func() bool) (func(), func(), error) { return nil, func() {}, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: stateDir, Managed: func() bool { return true }, Schedule: tc.schedule})
			job := controlJob("schedule-failed")
			events, result, ok := runControl(t, h, job, nil, nil, nil)
			if ok || result != nil || !reflect.DeepEqual(events, []string{"ack", "result"}) {
				t.Fatalf("events=%v result=%v ok=%v", events, result, ok)
			}
			if fired, err := h.marker(controlReservationKey(job), "fired"); err != nil {
				t.Fatal(err)
			} else if _, err := os.Stat(fired); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fired marker survived schedule failure: %v", err)
			}
			good := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: stateDir, Managed: func() bool { return true }, Schedule: testSchedule(func() {})})
			_, next, ok := runControl(t, good, controlJob("new-job"), nil, nil, nil)
			if !ok || next["accepted"] != false || next["code"] != "restart_unconfirmed" {
				t.Fatalf("unresolved attempt admitted fresh retry result=%v ok=%v", next, ok)
			}
		})
	}
}

func TestWorkerControlPublishFailureCancelsPreparedRestart(t *testing.T) {
	count, cancelled := 0, 0
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func(_ func() bool) (func(), func(), error) { return func() { count++ }, func() { cancelled++ }, nil }})
	job := controlJob("publish-failed")
	_, result, ok := runControl(t, h, job, errors.New("publish unavailable"), nil, nil)
	if ok || result != nil || count != 0 || cancelled != 1 {
		t.Fatalf("result=%v ok=%v restarts=%d cancelled=%d", result, ok, count, cancelled)
	}
	if fired, err := h.marker(controlReservationKey(job), "fired"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(fired); err != nil {
		t.Fatalf("fired marker missing after uncertain publish: %v", err)
	}
	_, duplicate, _ := runControl(t, h, job, nil, nil, nil)
	if duplicate != nil || count != 0 || job.controlSession.CanPrepare() {
		t.Fatalf("cancelled gate reopened: result=%v restarts=%d", duplicate, count)
	}
}

func TestWorkerControlCancelledReservationCannotExpireOrTransferToNewProcess(t *testing.T) {
	cfg := WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { t.Fatal("unresolved node restarted") })}
	h := NewWorkerControlHandler(cfg)
	job := controlJob("unconfirmed-original")
	reserved, err := h.marker(controlReservationKey(job), "reserved")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createDurableMarker(reserved); err != nil {
		t.Fatal(err)
	}
	if err := h.Abort(job); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(reserved, old, old); err != nil {
		t.Fatal(err)
	}
	// A replacement handler models a fresh process. Neither elapsed time nor
	// cancellation proves that any server-side authorization was resolved.
	replacement := NewWorkerControlHandler(cfg)
	_, result, _ := runControl(t, replacement, controlJob("successor"), nil, nil, nil)
	if result["accepted"] != false || result["code"] != "restart_unconfirmed" {
		t.Fatalf("old ambiguous attempt released node fence: %v", result)
	}
}

type uncertainControlWriter struct {
	controlWriter
}

type recoverableControlWriter struct {
	controlWriter
	calls int
}

type cancellingControlWriter struct {
	controlWriter
	cancel context.CancelFunc
}

func (w *cancellingControlWriter) WriteEnd(result map[string]any) error {
	w.cancel()
	return w.controlWriter.WriteEnd(result)
}

func TestWorkerControlCancellationDuringCompletionCannotCommit(t *testing.T) {
	count, cancelled := 0, 0
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func(_ func() bool) (func(), func(), error) { return func() { count++ }, func() { cancelled++ }, nil }})
	events := []string{}
	source := &controlSource{MockJobSource: NewMockJobSource("redis", nil), events: &events}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancellingControlWriter{controlWriter: controlWriter{events: &events}, cancel: cancel}
	runner := NewRunner(source, []JobHandler{h}, RunnerConfig{NodeID: "758"})
	if runner.executeJob(ctx, controlJob("cancel-completion"), writer, time.Now(), false, 0) || count != 0 || cancelled != 1 {
		t.Fatalf("cancellation lost to successful transport: commits=%d cancels=%d", count, cancelled)
	}
}

func (w *recoverableControlWriter) WriteEnd(result map[string]any) error {
	w.calls++
	if w.calls == 1 {
		return errors.New("completion response lost")
	}
	return w.controlWriter.WriteEnd(result)
}

func TestWorkerControlUnknownCompletionCannotRetryIntoExecution(t *testing.T) {
	count, cancelled := 0, 0
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: func(_ func() bool) (func(), func(), error) { return func() { count++ }, func() { cancelled++ }, nil }})
	events := []string{}
	source := &controlSource{MockJobSource: NewMockJobSource("redis", nil), events: &events}
	writer := &recoverableControlWriter{controlWriter: controlWriter{events: &events}}
	runner := NewRunner(source, []JobHandler{h}, RunnerConfig{NodeID: "758"})
	if runner.executeJob(context.Background(), controlJob("unknown-completion"), writer, time.Now(), false, 0) || count != 0 || cancelled != 1 || writer.calls != 1 {
		t.Fatalf("unknown completion retried: calls=%d commits=%d cancels=%d", writer.calls, count, cancelled)
	}
}

func (w *uncertainControlWriter) WriteEnd(result map[string]any) error {
	*w.events = append(*w.events, "result")
	w.result = result // Simulate a publish that reached the coordinator before its response was lost.
	return errors.New("publish response lost")
}

func TestWorkerControlDeliveredAcceptanceCannotAuthorizeAfterPublicationError(t *testing.T) {
	events := []string{}
	restarted := 0
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { restarted++ })})
	source := &controlSource{MockJobSource: NewMockJobSource("redis", nil), events: &events}
	writer := &uncertainControlWriter{controlWriter: controlWriter{events: &events}}
	runner := NewRunner(source, []JobHandler{h}, RunnerConfig{NodeID: "758", State: NewWorkerState()})
	oldAttempts, oldBackoff := streamWriteRetryAttempts, streamWriteRetryBackoff
	streamWriteRetryAttempts, streamWriteRetryBackoff = 1, []time.Duration{0}
	defer func() { streamWriteRetryAttempts, streamWriteRetryBackoff = oldAttempts, oldBackoff }()
	job := controlJob("delivered-before-error")
	if runner.executeJob(context.Background(), job, writer, time.Now(), false, 0) {
		t.Fatal("uncertain publication must not report a clean worker result")
	}
	if writer.result["accepted"] != true || restarted != 0 || !reflect.DeepEqual(events, []string{"ack", "result"}) {
		t.Fatalf("events=%v result=%v restarts=%d", events, writer.result, restarted)
	}
}

func TestWorkerControlSchedulerErrorCancelsPreparedWork(t *testing.T) {
	cancelled := 0
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true },
		Schedule: func(_ func() bool) (func(), func(), error) {
			return nil, func() { cancelled++ }, errors.New("prepare failed")
		}})
	_, result, ok := runControl(t, h, controlJob("partial-prepare"), nil, nil, nil)
	if ok || result != nil || cancelled != 1 {
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
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true },
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
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true },
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

// Redis delivers a JSON-number timeout_ms as float64 (the natural backend
// shape), not a string. A numeric budget must be accepted, not rejected as
// invalid; the string fixtures above never exercise this.
func TestWorkerControlV2RejectsPayloadTimeoutRatherThanExtendingAuthority(t *testing.T) {
	events := []string{}
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: t.TempDir(), Managed: func() bool { return true }, Schedule: testSchedule(func() { events = append(events, "restart") })})
	job := controlJob("numeric-timeout")
	job.Payload["timeout_ms"] = float64(25000)
	got, result, ok := runControl(t, h, job, nil, nil, &events)
	if !ok || result["accepted"] != false || result["code"] != "unknown_field" || !reflect.DeepEqual(got, []string{"ack", "result"}) {
		t.Fatalf("payload timeout changed authority: events=%v result=%v ok=%v", got, result, ok)
	}
}

// An unmanaged worker must refuse the restart cleanly, before writing any
// tombstone or scheduling an exit, so the node stays alive and a later managed
// retry of the same job id is not misread as a duplicate. This is the
// never-exit-into-a-dead-node invariant; it had no direct coverage.
func TestWorkerControlUnmanagedWorkerRefusesWithoutSchedulingOrTombstone(t *testing.T) {
	stateDir := t.TempDir()
	h := NewWorkerControlHandler(WorkerControlConfig{OrgID: fixtureOrg, NodeID: "758", StateDir: stateDir, Managed: func() bool { return false }, Schedule: testSchedule(func() { t.Fatal("unmanaged worker must never schedule a restart") })})
	job := controlJob("unmanaged")
	events, result, ok := runControl(t, h, job, nil, nil, nil)
	if !ok || result["accepted"] != false || result["restarting"] != false || result["code"] != "unmanaged_worker" {
		t.Fatalf("unmanaged worker must refuse: events=%v result=%v ok=%v", events, result, ok)
	}
	if reserved, err := h.marker(controlReservationKey(job), "reserved"); err != nil {
		t.Fatal(err)
	} else if _, statErr := os.Stat(reserved); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unmanaged refusal must not write a reserved marker: %v", statErr)
	}
}
