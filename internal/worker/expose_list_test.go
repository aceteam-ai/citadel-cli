package worker

import (
	"context"
	"errors"
	"testing"
)

func newExposeListJob(queue string, payload map[string]any) *Job {
	return &Job{ID: "j1", Type: JobTypeExposeList, SourceQueue: queue, Payload: payload}
}

func TestExposeList_RejectsSharedPool(t *testing.T) {
	ops := &fakeExposeOps{}
	h := NewExposeListHandler(ExposeListConfig{Ops: ops})
	job := newExposeListJob("jobs:v1:tag:gpu:rtx3090", nil)
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("shared-pool EXPOSE_LIST: got %s, want failure", res.Status)
	}
}

func TestExposeList_NilOps(t *testing.T) {
	h := NewExposeListHandler(ExposeListConfig{})
	job := newExposeListJob(exposePerNodeQueue, nil)
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("nil ops: got %s, want failure", res.Status)
	}
}

func TestExposeList_CanHandle(t *testing.T) {
	h := NewExposeListHandler(ExposeListConfig{})
	if !h.CanHandle(JobTypeExposeList) {
		t.Error("must handle EXPOSE_LIST")
	}
	if h.CanHandle(JobTypeExposeSet) {
		t.Error("must not handle EXPOSE_SET")
	}
}

// TestExposeList_EmptyPayloadListsEverything pins that no payload fields are
// required (design doc §3.1): an empty payload lists the whole durable set.
func TestExposeList_EmptyPayloadListsEverything(t *testing.T) {
	ops := &fakeExposeOps{listResult: &ExposeListResult{
		Exposures: []ExposureInfo{
			{Name: "frigate", Port: 5000, Visibility: "org", Epoch: 1, Live: true},
			{Name: "dash", Port: 6000, Visibility: "link", Epoch: 2, Live: false},
		},
		LiveOnly: []string{"scratch-dash"},
	}}
	h := NewExposeListHandler(ExposeListConfig{Ops: ops})
	job := newExposeListJob(exposePerNodeQueue, nil)
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("empty-payload EXPOSE_LIST: got %s (%v), want success", res.Status, res.Error)
	}
	if res.Output["count"] != 2 {
		t.Errorf("count: got %v, want 2", res.Output["count"])
	}
	exposures, ok := res.Output["exposures"].([]ExposureInfo)
	if !ok || len(exposures) != 2 {
		t.Fatalf("exposures output wrong shape: %#v", res.Output["exposures"])
	}
	liveOnly, ok := res.Output["live_only"].([]string)
	if !ok || len(liveOnly) != 1 || liveOnly[0] != "scratch-dash" {
		t.Errorf("live_only output wrong: %#v", res.Output["live_only"])
	}
}

// TestExposeList_CorruptStoreIsFailureNotEmptySuccess pins the design's
// explicit "a reader reporting an empty set as truth is exactly the
// blindness this job exists to end" rule (§3.2): a corrupt durable store must
// surface as a job FAILURE with the parse error, never a silent empty list.
func TestExposeList_CorruptStoreIsFailureNotEmptySuccess(t *testing.T) {
	ops := &fakeExposeOps{listErr: errors.New("parse exposures: unexpected end of JSON input")}
	h := NewExposeListHandler(ExposeListConfig{Ops: ops})
	job := newExposeListJob(exposePerNodeQueue, nil)
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("corrupt store: got %s, want failure", res.Status)
	}
	if res.Output["exposures"] != nil {
		t.Errorf("a failed list must not carry a partial/empty exposures list, got %#v", res.Output["exposures"])
	}
}
