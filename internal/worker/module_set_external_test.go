package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/externalengine"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
	"github.com/google/uuid"
)

func externalJob(status, rev, requestID string) *Job {
	return &Job{Type: JobTypeModuleSet, SourceQueue: "jobs:v1:shell:org_org1:node:12", Payload: map[string]any{
		"source": "vllm", "desired_status": status, "target_node": "12",
		"external_engine": map[string]any{"version": 1, "host": "127.0.0.1", "host_port": 58000, "model": "vendor/model", "revision": rev, "request_id": uuid.NewSHA1(uuid.NameSpaceDNS, []byte(requestID)).String()},
	}}
}

func TestExternalModuleLifecycleAndReplay(t *testing.T) {
	dir := t.TempDir()
	ops := newFakeModuleOps()
	probeCalls := 0
	cfg := &ExternalModuleConfig{NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops, Snapshot: func() error { return nil }, Probe: func(context.Context, externalengine.Endpoint, string) error { probeCalls++; return nil }}
	h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: cfg})
	apply := func(job *Job) *JobResult {
		r, err := h.Execute(context.Background(), job, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	bad := externalJob("adopt_external", "1", "one")
	bad.Payload["target_node"] = 12
	if r := apply(bad); r.Status != JobStatusFailure {
		t.Fatalf("numeric target: %+v", r)
	}
	bad = externalJob("adopt_external", "1", "one")
	bad.SourceQueue = "jobs:v1:shell:org_other:node:12"
	if r := apply(bad); r.Status != JobStatusFailure {
		t.Fatalf("wrong org: %+v", r)
	}
	if probeCalls != 0 {
		t.Fatal("probe before strict target rejection")
	}
	if r := apply(externalJob("adopt_external", "1", "one")); r.Status != JobStatusSuccess || r.Output["converged"] != false {
		t.Fatalf("adopt: %+v", r)
	}
	if r := apply(externalJob("adopt_external", "1", "one")); r.Status != JobStatusSuccess || r.Output["status"] != "already_applied" {
		t.Fatalf("duplicate: %+v", r)
	}
	if r := apply(externalJob("adopt_external", "1", "different")); r.Status != JobStatusFailure {
		t.Fatalf("conflict: %+v", r)
	}
	if r := apply(externalJob("adopt_external", "0", "old")); r.Status != JobStatusFailure {
		t.Fatalf("stale: %+v", r)
	}
	if probeCalls != 1 {
		t.Fatalf("probes = %d", probeCalls)
	}
	if r := apply(externalJob("detach_external", "2", "two")); r.Status != JobStatusSuccess {
		t.Fatalf("detach: %+v", r)
	}
	loaded, err := externalengine.Load(dir)
	if err != nil || loaded.Mode != "detached" || loaded.Model != "vendor/model" || loaded.Endpoint.Port != 58000 {
		t.Fatalf("detach state: %+v, %v", loaded, err)
	}
	for _, call := range ops.calls {
		if call != "list" {
			t.Fatalf("managed operation %q", call)
		}
	}
}

func TestExternalModuleFailedProbeAndOwnershipPreserveState(t *testing.T) {
	dir := t.TempDir()
	ops := newFakeModuleOps()
	cfg := &ExternalModuleConfig{NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops, Snapshot: func() error { return nil }, Probe: func(context.Context, externalengine.Endpoint, string) error { return errors.New("wrong model") }}
	h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: cfg})
	r, _ := h.Execute(context.Background(), externalJob("adopt_external", "1", "one"), nil)
	if r.Status != JobStatusFailure {
		t.Fatalf("probe result: %+v", r)
	}
	if state, err := externalengine.Load(dir); err != nil || state != nil {
		t.Fatalf("failed probe persisted: %+v %v", state, err)
	}
	ops.byName["vllm"] = reconcile.InstalledModule{Name: "vllm", Source: "vllm", Health: reconcile.HealthRunning}
	r, _ = h.Execute(context.Background(), externalJob("adopt_external", "1", "one"), nil)
	if r.Status != JobStatusFailure {
		t.Fatalf("managed conflict: %+v", r)
	}
}

func TestExternalModuleFailedWriteKeepsPreviousRecord(t *testing.T) {
	dir := t.TempDir()
	old, err := externalengine.Validate(externalengine.Config{Version: 1, Mode: "adopted", Endpoint: externalengine.Endpoint{Host: "127.0.0.1", Port: 58000}, Model: "vendor/model", Revision: "1", RequestID: "00000000-0000-4000-8000-000000000001", NodeID: "12"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := externalengine.Save(dir, old); err != nil {
		t.Fatal(err)
	}
	ops := newFakeModuleOps()
	cfg := &ExternalModuleConfig{NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops, Snapshot: func() error { return nil }, Probe: func(context.Context, externalengine.Endpoint, string) error { return nil }, Save: func(string, externalengine.Config) error { return errors.New("disk full") }}
	h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: cfg})
	r, _ := h.Execute(context.Background(), externalJob("adopt_external", "2", "two"), nil)
	if r.Status != JobStatusRetry {
		t.Fatalf("write failure: %+v", r)
	}
	got, err := externalengine.Load(dir)
	if err != nil || got.Revision != "1" || got.ConfigRef != old.ConfigRef {
		t.Fatalf("prior record changed: %+v %v", got, err)
	}
}

func TestExternalModuleCanDetachAfterPersistedAddressDisappears(t *testing.T) {
	dir := t.TempDir()
	old, err := externalengine.Validate(externalengine.Config{Version: 1, Mode: "adopted", Endpoint: externalengine.Endpoint{Host: "192.0.2.10", Port: 58000}, Model: "vendor/model", Revision: "1", RequestID: "00000000-0000-4000-8000-000000000001", NodeID: "12"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := externalengine.Save(dir, old); err != nil {
		t.Fatal(err)
	}
	ops := newFakeModuleOps()
	var logged bool
	cfg := &ExternalModuleConfig{
		NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops,
		Snapshot: func() error { return errors.New("host is not assigned to this node") },
		Probe:    func(context.Context, externalengine.Endpoint, string) error { return nil },
		Log:      func(string, ...any) { logged = true },
	}
	h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: cfg})
	r, _ := h.Execute(context.Background(), externalJob("detach_external", "2", "two"), nil)
	if r.Status != JobStatusSuccess || !logged {
		t.Fatalf("detach recovery: %+v, logged=%v", r, logged)
	}
	loaded, err := externalengine.Load(dir)
	if err != nil || loaded == nil || loaded.Mode != "detached" || loaded.Revision != "2" {
		t.Fatalf("detached state: %+v, %v", loaded, err)
	}
	r, _ = h.Execute(context.Background(), externalJob("adopt_external", "3", "three"), nil)
	if r.Status != JobStatusSuccess {
		t.Fatalf("adopt recovery: %+v", r)
	}
	loaded, err = externalengine.Load(dir)
	if err != nil || loaded == nil || loaded.Mode != "adopted" || loaded.Revision != "3" || loaded.Endpoint.Host != "127.0.0.1" {
		t.Fatalf("adopted state: %+v, %v", loaded, err)
	}
}

func TestManagedVLLMActionRefusedWhileExternalRecordExists(t *testing.T) {
	for _, mode := range []string{"adopted", "detached"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			stored, err := externalengine.Validate(externalengine.Config{Version: 1, Mode: mode, Endpoint: externalengine.Endpoint{Host: "127.0.0.1", Port: 58000}, Model: "vendor/model", Revision: "1", RequestID: "00000000-0000-4000-8000-000000000001", NodeID: "12"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := externalengine.Save(dir, stored); err != nil {
				t.Fatal(err)
			}
			ops := newFakeModuleOps()
			h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: &ExternalModuleConfig{NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops}})
			for _, status := range []string{"running", "stopped", "absent"} {
				r, err := h.Execute(context.Background(), &Job{Type: JobTypeModuleSet, SourceQueue: "jobs:v1:shell:org_org1:node:12", Payload: map[string]any{"source": "vllm", "desired_status": status}}, nil)
				if err != nil || r.Status != JobStatusFailure {
					t.Fatalf("status %s: %+v, %v", status, r, err)
				}
			}
			if len(ops.calls) != 0 {
				t.Fatalf("managed effects under external ownership: %v", ops.calls)
			}
		})
	}
}

func TestExternalModulePortOnlyDefaultsLoopback(t *testing.T) {
	dir := t.TempDir()
	ops := newFakeModuleOps()
	cfg := &ExternalModuleConfig{NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops, Snapshot: func() error { return nil }, Probe: func(_ context.Context, e externalengine.Endpoint, _ string) error {
		if e.Host != "127.0.0.1" {
			t.Fatalf("host=%q", e.Host)
		}
		return nil
	}}
	h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: cfg})
	job := externalJob("adopt_external", "1", "one")
	delete(job.Payload["external_engine"].(map[string]any), "host")
	r, _ := h.Execute(context.Background(), job, nil)
	if r.Status != JobStatusSuccess {
		t.Fatalf("port-only: %+v", r)
	}
}

func TestExternalWirePersistProbeAndInference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"vendor/model"}]}`)
		case "/v1/completions":
			var req struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "vendor/model" {
				http.Error(w, "wrong model", 400)
				return
			}
			if req.Stream {
				fmt.Fprint(w, "data: {\"choices\":[{\"text\":\"streamed\"}]}\n\ndata: [DONE]\n\n")
			} else {
				fmt.Fprint(w, `{"choices":[{"text":"buffered","finish_reason":"stop"}]}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	if parsed := net.ParseIP(u.Hostname()); parsed == nil || !parsed.IsLoopback() {
		t.Fatalf("test server is not loopback: %q", u.Hostname())
	}
	// This is the exact JSON envelope the AceTeam action service serializes.
	raw := fmt.Sprintf(`{"source":"vllm","desired_status":"adopt_external","target_node":"12","external_engine":{"version":1,"host":%q,"host_port":%d,"model":"vendor/model","revision":"7","request_id":"00000000-0000-4000-8000-000000000007"}}`, u.Hostname(), port)
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ops := newFakeModuleOps()
	h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: &ExternalModuleConfig{NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops, Snapshot: func() error { return nil }}})
	result, err := h.Execute(context.Background(), &Job{Type: JobTypeModuleSet, SourceQueue: "jobs:v1:shell:org_org1:node:12", Payload: payload}, nil)
	if err != nil || result.Status != JobStatusSuccess {
		t.Fatalf("apply: %+v, %v", result, err)
	}
	loaded, err := externalengine.Load(dir)
	if err != nil || loaded == nil {
		t.Fatalf("boot load: %+v, %v", loaded, err)
	}
	if err := externalengine.Probe(context.Background(), loaded.Endpoint, loaded.Model); err != nil {
		t.Fatalf("fresh observed model: %v", err)
	}
	inference := &LLMInferenceHandler{baseURLs: map[string]string{"vllm": loaded.Endpoint.BaseURL()}, httpClient: &http.Client{Transport: &http.Transport{Proxy: nil}}}
	for _, streamed := range []bool{false, true} {
		response, err := inference.executeVLLM(context.Background(), &NoOpStreamWriter{}, &jobs.LLMInferencePayload{Model: loaded.Model, Prompt: "hello", Stream: streamed}, "test")
		want := "buffered"
		if streamed {
			want = "streamed"
		}
		if err != nil || response.Status != JobStatusSuccess || response.Output["content"] != want {
			t.Fatalf("inference stream=%v: %+v, %v", streamed, response, err)
		}
	}
}

func TestExternalRestartWaitsForAckAndResumesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	ops := newFakeModuleOps()
	var active atomic.Int32
	active.Store(1)
	drained := false
	resumed := make(chan struct{})
	restarted := make(chan struct{})
	cfg := &ExternalModuleConfig{
		NodeID: "12", OrgID: "org1", Dir: dir, Ops: ops,
		Snapshot: func() error { return nil },
		Probe:    func(context.Context, externalengine.Endpoint, string) error { return nil },
		BeginDrain: func() func() {
			drained = true
			return func() { close(resumed) }
		},
		ActiveJobs:  func() int { return int(active.Load()) },
		Restart:     func() error { close(restarted); return errors.New("reexec failed") },
		IdleTimeout: time.Second,
	}
	h := NewModuleSetHandler(ModuleSetConfig{Ops: ops, External: cfg})
	r, _ := h.Execute(context.Background(), externalJob("adopt_external", "1", "one"), nil)
	if r.Status != JobStatusSuccess || !drained {
		t.Fatalf("result/drain: %+v %v", r, drained)
	}
	select {
	case <-restarted:
		t.Fatal("restarted before ack")
	case <-time.After(150 * time.Millisecond):
	}
	active.Store(0) // the real runner decrements only after WriteEnd and Ack
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("did not restart after ack")
	}
	select {
	case <-resumed:
	case <-time.After(time.Second):
		t.Fatal("failed restart left runner drained")
	}
	if state, err := externalengine.Load(dir); err != nil || state != nil {
		t.Fatalf("failed restart did not restore prior config: %+v, %v", state, err)
	}
}
