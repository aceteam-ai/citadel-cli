package worker

import (
	"context"
	"errors"
	"testing"
)

// fakeAppOps records calls and returns canned results/errors.
type fakeAppOps struct {
	deployReq   AppDeployRequest
	deployCalls int
	deployRes   *AppDeployResult
	deployErr   error

	lastVerb  string
	lastShort string
	lastTail  int
	lifeErr   error
	statusRes *AppStatusResult
	logsRes   *AppLogsResult
}

func (f *fakeAppOps) Deploy(_ context.Context, req AppDeployRequest) (*AppDeployResult, error) {
	f.deployCalls++
	f.deployReq = req
	if f.deployErr != nil {
		return nil, f.deployErr
	}
	if f.deployRes != nil {
		return f.deployRes, nil
	}
	return &AppDeployResult{Name: "app-" + req.ShortCode, ShortCode: req.ShortCode, State: "starting", HostPort: 18900}, nil
}
func (f *fakeAppOps) Stop(_ context.Context, sc string) (*AppLifecycleResult, error) {
	f.lastVerb, f.lastShort = "stop", sc
	return &AppLifecycleResult{ShortCode: sc, State: "stopped"}, f.lifeErr
}
func (f *fakeAppOps) Start(_ context.Context, sc string) (*AppLifecycleResult, error) {
	f.lastVerb, f.lastShort = "start", sc
	return &AppLifecycleResult{ShortCode: sc, State: "running"}, f.lifeErr
}
func (f *fakeAppOps) Status(_ context.Context, sc string) (*AppStatusResult, error) {
	f.lastVerb, f.lastShort = "status", sc
	if f.statusRes != nil {
		return f.statusRes, f.lifeErr
	}
	return &AppStatusResult{ShortCode: sc, State: "running"}, f.lifeErr
}
func (f *fakeAppOps) Logs(_ context.Context, sc string, tail int) (*AppLogsResult, error) {
	f.lastVerb, f.lastShort, f.lastTail = "logs", sc, tail
	if f.logsRes != nil {
		return f.logsRes, f.lifeErr
	}
	return &AppLogsResult{ShortCode: sc, Logs: "log lines"}, f.lifeErr
}
func (f *fakeAppOps) Destroy(_ context.Context, sc string) (*AppLifecycleResult, error) {
	f.lastVerb, f.lastShort = "destroy", sc
	return &AppLifecycleResult{ShortCode: sc, State: "destroyed"}, f.lifeErr
}

func appJob(jobType, queue string, payload map[string]any) *Job {
	return &Job{ID: "app-job-1", Type: jobType, SourceQueue: queue, Payload: payload}
}

func validDeployPayload() map[string]any {
	return map[string]any{
		"short_code":     "ac-blue-cat-fox",
		"image":          "ghcr.io/aceteam-ai/streamlit-runtime:latest",
		"container_port": 8501,
		"visibility":     "org",
	}
}

func TestAppHandler_CanHandle(t *testing.T) {
	h := NewAppHandler(AppHandlerConfig{})
	for _, jt := range []string{JobTypeAppDeploy, JobTypeAppStop, JobTypeAppStart, JobTypeAppStatus, JobTypeAppLogs, JobTypeAppDestroy} {
		if !h.CanHandle(jt) {
			t.Errorf("CanHandle(%q) = false, want true", jt)
		}
	}
	if h.CanHandle(JobTypeExposeSet) {
		t.Error("CanHandle(EXPOSE_SET) = true, want false")
	}
}

func TestAppHandler_RejectsSharedPool(t *testing.T) {
	ops := &fakeAppOps{}
	h := NewAppHandler(AppHandlerConfig{Ops: ops})
	res, _ := h.Execute(context.Background(), appJob(JobTypeAppDeploy, "jobs:v1:tag:gpu:rtx3090", validDeployPayload()), nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("shared-pool APP_DEPLOY: got %s, want failure", res.Status)
	}
	if ops.deployCalls != 0 {
		t.Error("ops must not be called for a shared-pool job")
	}
}

func TestAppHandler_NilOps(t *testing.T) {
	h := NewAppHandler(AppHandlerConfig{})
	res, _ := h.Execute(context.Background(), appJob(JobTypeAppStatus, exposePerNodeQueue, map[string]any{"short_code": "ac-x"}), nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("nil ops: got %s, want failure", res.Status)
	}
}

func TestAppHandler_DeployValidation(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"missing short_code", map[string]any{"image": "ghcr.io/aceteam-ai/x:1", "container_port": 8501}},
		{"missing image", map[string]any{"short_code": "ac-x", "container_port": 8501}},
		{"privileged container_port", map[string]any{"short_code": "ac-x", "image": "ghcr.io/aceteam-ai/x:1", "container_port": 80}},
		{"unknown visibility", map[string]any{"short_code": "ac-x", "image": "ghcr.io/aceteam-ai/x:1", "container_port": 8501, "visibility": "public"}},
		{"platform visibility refused (A5)", map[string]any{"short_code": "ac-x", "image": "ghcr.io/aceteam-ai/x:1", "container_port": 8501, "visibility": "platform"}},
		{"git source refused (A2)", map[string]any{"short_code": "ac-x", "image": "ghcr.io/aceteam-ai/x:1", "container_port": 8501, "source_kind": "git"}},
		{"bad short_code chars", map[string]any{"short_code": "AC_X", "image": "ghcr.io/aceteam-ai/x:1", "container_port": 8501}},
		{"empty payload", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ops := &fakeAppOps{}
			h := NewAppHandler(AppHandlerConfig{Ops: ops})
			res, _ := h.Execute(context.Background(), appJob(JobTypeAppDeploy, exposePerNodeQueue, c.payload), nil)
			if res.Status != JobStatusFailure {
				t.Errorf("%s: got %s, want failure", c.name, res.Status)
			}
			if ops.deployCalls != 0 {
				t.Errorf("%s: ops.Deploy must not be called on a bad payload", c.name)
			}
		})
	}
}

func TestAppHandler_DeploySuccess(t *testing.T) {
	ops := &fakeAppOps{deployRes: &AppDeployResult{
		Name: "app-ac-blue-cat-fox", ShortCode: "ac-blue-cat-fox",
		URL: "https://100.64.0.9:8443/expose/app-ac-blue-cat-fox", HostPort: 18900,
		PodID: "pod123", State: "starting", Visibility: "org", Epoch: 1,
	}}
	h := NewAppHandler(AppHandlerConfig{Ops: ops})
	res, _ := h.Execute(context.Background(), appJob(JobTypeAppDeploy, exposePerNodeQueue, validDeployPayload()), nil)
	if res.Status != JobStatusSuccess {
		t.Fatalf("APP_DEPLOY: got %s (%v), want success", res.Status, res.Error)
	}
	// JobID is set from the Job, not the payload.
	if ops.deployReq.JobID != "app-job-1" {
		t.Errorf("Deploy req JobID = %q, want app-job-1", ops.deployReq.JobID)
	}
	// Defaults applied by the parser.
	if ops.deployReq.Visibility != "org" || ops.deployReq.SourceKind != "image" {
		t.Errorf("defaults: visibility=%q source_kind=%q", ops.deployReq.Visibility, ops.deployReq.SourceKind)
	}
	if res.Output["state"] != "starting" || res.Output["host_port"].(float64) != 18900 {
		t.Errorf("output shape: %+v", res.Output)
	}
}

func TestAppHandler_DeployTransientRetries(t *testing.T) {
	// A transient error wraps ErrAppTransient so errors.Is matches (mirrors how
	// liveAppOps signals "no gateway yet").
	ops := &fakeAppOps{deployErr: errWrap{ErrAppTransient}}
	h := NewAppHandler(AppHandlerConfig{Ops: ops})
	res, _ := h.Execute(context.Background(), appJob(JobTypeAppDeploy, exposePerNodeQueue, validDeployPayload()), nil)
	if res.Status != JobStatusRetry {
		t.Fatalf("transient deploy: got %s, want retry", res.Status)
	}
}

type errWrap struct{ err error }

func (e errWrap) Error() string { return "no gateway: " + e.err.Error() }
func (e errWrap) Unwrap() error { return e.err }

func TestAppHandler_DeployTerminalFails(t *testing.T) {
	ops := &fakeAppOps{deployErr: errors.New("image not in an allowed registry")}
	h := NewAppHandler(AppHandlerConfig{Ops: ops})
	res, _ := h.Execute(context.Background(), appJob(JobTypeAppDeploy, exposePerNodeQueue, validDeployPayload()), nil)
	if res.Status != JobStatusFailure {
		t.Fatalf("terminal deploy error: got %s, want failure", res.Status)
	}
}

func TestAppHandler_LifecycleDispatch(t *testing.T) {
	cases := []struct {
		jobType string
		verb    string
	}{
		{JobTypeAppStop, "stop"},
		{JobTypeAppStart, "start"},
		{JobTypeAppStatus, "status"},
		{JobTypeAppLogs, "logs"},
		{JobTypeAppDestroy, "destroy"},
	}
	for _, c := range cases {
		t.Run(c.verb, func(t *testing.T) {
			ops := &fakeAppOps{}
			h := NewAppHandler(AppHandlerConfig{Ops: ops})
			res, _ := h.Execute(context.Background(), appJob(c.jobType, exposePerNodeQueue, map[string]any{"short_code": "ac-blue-cat-fox"}), nil)
			if res.Status != JobStatusSuccess {
				t.Fatalf("%s: got %s (%v), want success", c.verb, res.Status, res.Error)
			}
			if ops.lastVerb != c.verb || ops.lastShort != "ac-blue-cat-fox" {
				t.Errorf("%s: dispatched verb=%q short=%q", c.verb, ops.lastVerb, ops.lastShort)
			}
		})
	}
}

func TestAppHandler_LogsDefaultTail(t *testing.T) {
	ops := &fakeAppOps{}
	h := NewAppHandler(AppHandlerConfig{Ops: ops})
	h.Execute(context.Background(), appJob(JobTypeAppLogs, exposePerNodeQueue, map[string]any{"short_code": "ac-x"}), nil)
	if ops.lastTail != defaultAppLogTail {
		t.Errorf("default tail = %d, want %d", ops.lastTail, defaultAppLogTail)
	}
}

func TestAppHandler_StopTransientRetries(t *testing.T) {
	ops := &fakeAppOps{lifeErr: errors.New("engine busy")}
	h := NewAppHandler(AppHandlerConfig{Ops: ops})
	res, _ := h.Execute(context.Background(), appJob(JobTypeAppStop, exposePerNodeQueue, map[string]any{"short_code": "ac-x"}), nil)
	if res.Status != JobStatusRetry {
		t.Fatalf("stop engine error: got %s, want retry", res.Status)
	}
}

func TestIsValidAppShortCode(t *testing.T) {
	good := []string{"ac-blue-cat-fox", "app123", "a", "ac-1-2-3"}
	bad := []string{"", "-lead", "trail-", "ac--x", "AC-X", "ac_x", "ac.x", "ac x"}
	for _, s := range good {
		if !isValidAppShortCode(s) {
			t.Errorf("isValidAppShortCode(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isValidAppShortCode(s) {
			t.Errorf("isValidAppShortCode(%q) = true, want false", s)
		}
	}
}

// TestExposeVerbsRefuseAppPrefix pins that an operator cannot mint or tear down
// a reserved "app-" route through the EXPOSE_SET / UNEXPOSE parsers (CRAM A1).
func TestExposeVerbsRefuseAppPrefix(t *testing.T) {
	if _, err := parseExposeRequest(map[string]any{"name": "app-ac-x", "port": 5000, "visibility": "org"}); err == nil {
		t.Error("parseExposeRequest accepted a reserved app- name")
	}
	if _, err := parseUnexposeRequest(map[string]any{"name": "app-ac-x"}); err == nil {
		t.Error("parseUnexposeRequest accepted a reserved app- name")
	}
	// A normal name still works.
	if _, err := parseExposeRequest(map[string]any{"name": "frigate", "port": 5000, "visibility": "org"}); err != nil {
		t.Errorf("parseExposeRequest rejected a normal name: %v", err)
	}
}
