package worker

import (
	"context"
	"errors"
	"testing"
)

func newUnexposeJob(queue string, payload map[string]any) *Job {
	return &Job{ID: "j1", Type: JobTypeUnexpose, SourceQueue: queue, Payload: payload}
}

func TestUnexpose_RejectsSharedPool(t *testing.T) {
	ops := &fakeExposeOps{}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob("jobs:v1:tag:gpu:rtx3090", map[string]any{"name": "frigate"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("shared-pool UNEXPOSE: got %s, want failure", res.Status)
	}
}

func TestUnexpose_NilOps(t *testing.T) {
	h := NewUnexposeHandler(UnexposeConfig{})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{"name": "frigate"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("nil ops: got %s, want failure", res.Status)
	}
}

func TestUnexpose_MissingName(t *testing.T) {
	ops := &fakeExposeOps{}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("missing name: got %s, want failure", res.Status)
	}
}

// TestUnexpose_IdempotentWasExposedFalseIsSuccess pins the design's explicit
// contract (§4.2): revoking a name that was never live is still a SUCCESS,
// just with was_exposed:false — the same idempotent-revoke contract the CLI
// already presents (service_unexpose.go).
func TestUnexpose_IdempotentWasExposedFalseIsSuccess(t *testing.T) {
	ops := &fakeExposeOps{unexposeResult: &UnexposeResult{Name: "frigate", WasExposed: false}}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{"name": "frigate"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("idempotent unexpose: got %s (%v), want success", res.Status, res.Error)
	}
	if res.Output["was_exposed"] != false {
		t.Errorf("was_exposed: got %v, want false", res.Output["was_exposed"])
	}
	// "removed" is the platform wire contract (aceteam PR #8631): its parser
	// defaults removed=True whenever the key is ABSENT, so the idempotent
	// not-currently-exposed case must explicitly send removed=false or the
	// platform misreports it as removed=true.
	if res.Output["removed"] != false {
		t.Errorf("removed: got %v, want false", res.Output["removed"])
	}
	if res.Output["name"] != "frigate" {
		t.Errorf("name: got %v, want frigate", res.Output["name"])
	}
}

func TestUnexpose_Success(t *testing.T) {
	ops := &fakeExposeOps{unexposeResult: &UnexposeResult{Name: "frigate", WasExposed: true}}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{"name": "frigate"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("unexpose: got %s (%v), want success", res.Status, res.Error)
	}
	if res.Output["was_exposed"] != true {
		t.Errorf("was_exposed: got %v, want true", res.Output["was_exposed"])
	}
	if res.Output["removed"] != true {
		t.Errorf("removed: got %v, want true", res.Output["removed"])
	}
}

// TestUnexpose_StaleDurableRecordWithNoLiveRouteReportsRemoved pins citadel
// #967: a durable exposure record can outlive its live route (restored for a
// port that no longer listens, or written by an older build). Unexpose still
// deletes that record even when nothing was live (WasExposed=false), and the
// handler must report removed=true for that case — something WAS cleaned up
// — while "was_exposed" keeps its original, narrower meaning unchanged.
func TestUnexpose_StaleDurableRecordWithNoLiveRouteReportsRemoved(t *testing.T) {
	ops := &fakeExposeOps{unexposeResult: &UnexposeResult{
		Name:                 "frigate",
		WasExposed:           false,
		DurableRecordRemoved: true,
	}}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{"name": "frigate"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("stale-record unexpose: got %s (%v), want success", res.Status, res.Error)
	}
	if res.Output["was_exposed"] != false {
		t.Errorf("was_exposed: got %v, want false (no live route existed)", res.Output["was_exposed"])
	}
	if res.Output["removed"] != true {
		t.Errorf("removed: got %v, want true (the stale durable record was cleaned up)", res.Output["removed"])
	}
}

// TestUnexpose_GenuinelyAbsentNameReportsNotRemoved is the negative control
// for the above: a name with no live route AND no durable record must still
// report removed=false, so "removed" doesn't degenerate into always-true.
func TestUnexpose_GenuinelyAbsentNameReportsNotRemoved(t *testing.T) {
	ops := &fakeExposeOps{unexposeResult: &UnexposeResult{
		Name:                 "nonexistent",
		WasExposed:           false,
		DurableRecordRemoved: false,
	}}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{"name": "nonexistent"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("absent-name unexpose: got %s (%v), want success", res.Status, res.Error)
	}
	if res.Output["removed"] != false {
		t.Errorf("removed: got %v, want false (nothing existed to clean up)", res.Output["removed"])
	}
}

// TestUnexpose_NoGatewayRetries mirrors EXPOSE_SET's retry-vs-failure split
// (design doc §4.2): "no in-process gateway" is transient and retried.
func TestUnexpose_NoGatewayRetries(t *testing.T) {
	ops := &fakeExposeOps{unexposeErr: errors.New("no in-process gateway (unexpose requires the node gateway to be running)")}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{"name": "frigate"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusRetry {
		t.Fatalf("no-gateway: got %s, want retry", res.Status)
	}
}

// TestUnexpose_DurableDeleteFailureIsFailureNotRetry mirrors design doc §4.2:
// once the live route is already down, a durable-delete failure must be a
// terminal FAILURE, not a retry that would just re-run the (already
// succeeded) gateway teardown forever.
func TestUnexpose_DurableDeleteFailureIsFailureNotRetry(t *testing.T) {
	ops := &fakeExposeOps{unexposeErr: errors.New(
		`exposure "frigate" is no longer served, but its saved record could not be removed (it will return on the next restart): write exposures: disk full`)}
	h := NewUnexposeHandler(UnexposeConfig{Ops: ops})
	job := newUnexposeJob(exposePerNodeQueue, map[string]any{"name": "frigate"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("durable-delete failure: got %s, want failure (not retry)", res.Status)
	}
}

func TestUnexpose_CanHandle(t *testing.T) {
	h := NewUnexposeHandler(UnexposeConfig{})
	if !h.CanHandle(JobTypeUnexpose) {
		t.Error("must handle UNEXPOSE")
	}
	if h.CanHandle(JobTypeExposeSet) {
		t.Error("must not handle EXPOSE_SET")
	}
}
