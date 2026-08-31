package worker

import (
	"context"
	"testing"
)

// fakeExposeOps records the request it was given and returns a canned result (or
// a forced error), so the handler's routing/validation is tested without a live
// gateway.
type fakeExposeOps struct {
	got    ExposeRequest
	called bool
	result *ExposeResult
	err    error

	unexposeResult *UnexposeResult
	unexposeErr    error
	listResult     *ExposeListResult
	listErr        error
}

func (f *fakeExposeOps) Expose(_ context.Context, req ExposeRequest) (*ExposeResult, error) {
	f.called = true
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &ExposeResult{URL: "https://100.64.0.9:8443/expose/" + req.Name}, nil
}

func (f *fakeExposeOps) Unexpose(_ context.Context, name string) (*UnexposeResult, error) {
	if f.unexposeErr != nil {
		return nil, f.unexposeErr
	}
	if f.unexposeResult != nil {
		return f.unexposeResult, nil
	}
	return &UnexposeResult{Name: name, WasExposed: true}, nil
}

func (f *fakeExposeOps) List(_ context.Context) (*ExposeListResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResult != nil {
		return f.listResult, nil
	}
	return &ExposeListResult{}, nil
}

const exposePerNodeQueue = "jobs:v1:shell:org_test:node:1314"

func newExposeJob(queue string, payload map[string]any) *Job {
	return &Job{ID: "j1", Type: JobTypeExposeSet, SourceQueue: queue, Payload: payload}
}

func TestExposeSet_RejectsSharedPool(t *testing.T) {
	ops := &fakeExposeOps{}
	h := NewExposeSetHandler(ExposeSetConfig{Ops: ops})
	job := newExposeJob("jobs:v1:tag:gpu:rtx3090", map[string]any{"name": "frigate", "port": 5000, "visibility": "org"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("shared-pool EXPOSE_SET: got %s, want failure", res.Status)
	}
	if ops.called {
		t.Error("ops must not be called for a shared-pool job")
	}
}

func TestExposeSet_NilOps(t *testing.T) {
	h := NewExposeSetHandler(ExposeSetConfig{})
	job := newExposeJob(exposePerNodeQueue, map[string]any{"name": "frigate", "port": 5000, "visibility": "org"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("nil ops: got %s, want failure", res.Status)
	}
}

func TestExposeSet_ValidatesPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"missing name", map[string]any{"port": 5000, "visibility": "org"}},
		{"missing port and path", map[string]any{"name": "frigate", "visibility": "org"}},
		{"zero port and no path", map[string]any{"name": "frigate", "port": 0, "visibility": "org"}},
		{"bad visibility", map[string]any{"name": "frigate", "port": 5000, "visibility": "public"}},
		{"empty payload", nil},
		// #943: port and path are mutually exclusive source types.
		{"both port and path", map[string]any{"name": "frigate", "port": 5000, "path": "results/ocr", "visibility": "org"}},
		{"blank path and no port", map[string]any{"name": "frigate", "path": "   ", "visibility": "org"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ops := &fakeExposeOps{}
			h := NewExposeSetHandler(ExposeSetConfig{Ops: ops})
			res, _ := h.Execute(context.Background(), newExposeJob(exposePerNodeQueue, c.payload), nil)
			if res.Status != JobStatusFailure {
				t.Errorf("%s: got %s, want failure", c.name, res.Status)
			}
			if ops.called {
				t.Errorf("%s: ops must not be called on a bad payload", c.name)
			}
		})
	}
}

func TestExposeSet_Success(t *testing.T) {
	ops := &fakeExposeOps{result: &ExposeResult{URL: "https://100.64.0.9:8443/expose/frigate", Token: "tok", ExpiresAt: "2026-01-01T00:00:00Z"}}
	h := NewExposeSetHandler(ExposeSetConfig{Ops: ops})
	job := newExposeJob(exposePerNodeQueue, map[string]any{
		"name": "frigate", "port": 5000, "visibility": "link", "ttl_seconds": 3600,
	})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("valid EXPOSE_SET: got %s (%v), want success", res.Status, res.Error)
	}
	if !ops.called {
		t.Fatal("ops.Expose was not called")
	}
	// Epoch defaults to 1 when omitted.
	if ops.got.Epoch != 1 {
		t.Errorf("epoch: got %d, want default 1", ops.got.Epoch)
	}
	if ops.got.Name != "frigate" || ops.got.Port != 5000 || ops.got.Visibility != "link" {
		t.Errorf("request passthrough wrong: %+v", ops.got)
	}
	if res.Output["url"] != "https://100.64.0.9:8443/expose/frigate" {
		t.Errorf("url output: got %v", res.Output["url"])
	}
	if res.Output["token"] != "tok" {
		t.Errorf("token output: got %v", res.Output["token"])
	}
}

// TestExposeSet_PathSource_Success proves the directory-source EXPOSE_SET
// payload (issue #943) routes through cleanly: no port required, and the raw
// path is passed to ExposeOps for the cmd-layer to workspace-confine (this
// package deliberately does not know what "the workspace" is).
func TestExposeSet_PathSource_Success(t *testing.T) {
	ops := &fakeExposeOps{result: &ExposeResult{URL: "https://100.64.0.9:8443/expose/scans"}}
	h := NewExposeSetHandler(ExposeSetConfig{Ops: ops})
	job := newExposeJob(exposePerNodeQueue, map[string]any{
		"name": "scans", "path": "results/ocr", "visibility": "link", "ttl_seconds": 3600,
	})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("valid path-source EXPOSE_SET: got %s (%v), want success", res.Status, res.Error)
	}
	if ops.got.Path != "results/ocr" {
		t.Errorf("path passthrough: got %q, want %q", ops.got.Path, "results/ocr")
	}
	if ops.got.Port != 0 {
		t.Errorf("port must stay zero for a path source, got %d", ops.got.Port)
	}
}

func TestExposeSet_OpsErrorRetries(t *testing.T) {
	ops := &fakeExposeOps{err: context.DeadlineExceeded}
	h := NewExposeSetHandler(ExposeSetConfig{Ops: ops})
	job := newExposeJob(exposePerNodeQueue, map[string]any{"name": "frigate", "port": 5000, "visibility": "org"})
	res, _ := h.Execute(context.Background(), job, nil)
	if res.Status != JobStatusRetry {
		t.Fatalf("ops error: got %s, want retry", res.Status)
	}
}

func TestExposeSet_CanHandle(t *testing.T) {
	h := NewExposeSetHandler(ExposeSetConfig{})
	if !h.CanHandle(JobTypeExposeSet) {
		t.Error("must handle EXPOSE_SET")
	}
	if h.CanHandle(JobTypeModuleSet) {
		t.Error("must not handle MODULE_SET")
	}
}
