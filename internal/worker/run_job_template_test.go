package worker

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

type fakeTemplateOps struct {
	called bool
	result *TemplateRunResult
	err    error
}

func (f *fakeTemplateOps) Run(ctx context.Context, req TemplateRunRequest) (*TemplateRunResult, error) {
	f.called = true
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

const (
	tmplPerNodeQueue   = "jobs:v1:shell:org_test:node:1008"
	tmplSharedOrgQueue = "jobs:v1:shell:org_test"
	tmplBuiltinRunner  = `{"kind":"builtin","handler":"audio-mix"}`
	tmplInSchema       = `{"type":"object","properties":{"gain":{"type":"number"}}}`
	tmplOutSchema      = `{"type":"object","properties":{"mix":{"type":"string"}}}`
	tmplKey            = "audio-mix"
	tmplVersion        = 2
)

// tmplPayload builds a valid RUN_JOB_TEMPLATE payload whose content_hash is the
// true recomputed hash, so the anti-tamper check passes unless a test
// deliberately corrupts a field.
func tmplPayload(t *testing.T, runner, inSchema, outSchema string) map[string]any {
	t.Helper()
	hash, err := jobs.ComputeTemplateManifestHash(tmplKey, tmplVersion,
		json.RawMessage(inSchema), json.RawMessage(outSchema), json.RawMessage(runner))
	if err != nil {
		t.Fatalf("ComputeTemplateManifestHash: %v", err)
	}
	return map[string]any{
		"template_key":     tmplKey,
		"template_version": strconv.Itoa(tmplVersion),
		"content_hash":     hash,
		"runner":           runner,
		"input_schema":     inSchema,
		"output_schema":    outSchema,
		"params":           `{"gain":1.5}`,
		"input_files":      `[]`,
	}
}

func tmplRunJob(ops TemplateRunOps, queue string, payload map[string]any) (*JobResult, error) {
	h := NewRunJobTemplateHandler(RunJobTemplateHandlerConfig{Ops: ops})
	job := &Job{ID: "job-1", Type: JobTypeRunJobTemplate, SourceQueue: queue, Payload: payload}
	return h.Execute(context.Background(), job, nil)
}

func TestRunJobTemplate_RefusalPathsNeverRunOps(t *testing.T) {
	cases := []struct {
		name    string
		queue   string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name:    "shared_org_stream_refused",
			queue:   tmplSharedOrgQueue,
			wantErr: "per-node stream",
		},
		{
			name:    "hash_mismatch_refused",
			queue:   tmplPerNodeQueue,
			mutate:  func(p map[string]any) { p["content_hash"] = "00deadbeef" },
			wantErr: "does not match approved content_hash",
		},
		{
			name:    "shell_kind_refused",
			queue:   tmplPerNodeQueue,
			mutate:  func(p map[string]any) { rehashRunner(t, p, `{"kind":"shell","handler":"sh"}`) },
			wantErr: "shell execution is never allowed",
		},
		{
			name:    "unknown_kind_refused",
			queue:   tmplPerNodeQueue,
			mutate:  func(p map[string]any) { rehashRunner(t, p, `{"kind":"docker","handler":"x"}`) },
			wantErr: "is not permitted",
		},
		{
			name:    "empty_handler_refused",
			queue:   tmplPerNodeQueue,
			mutate:  func(p map[string]any) { rehashRunner(t, p, `{"kind":"builtin","handler":""}`) },
			wantErr: "no handler name",
		},
		{
			name:    "missing_input_schema_refused",
			queue:   tmplPerNodeQueue,
			mutate:  func(p map[string]any) { delete(p, "input_schema") },
			wantErr: "missing input_schema",
		},
		{
			name:    "missing_content_hash_refused",
			queue:   tmplPerNodeQueue,
			mutate:  func(p map[string]any) { delete(p, "content_hash") },
			wantErr: "missing content_hash",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeTemplateOps{result: &TemplateRunResult{}}
			payload := tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema)
			if tc.mutate != nil {
				tc.mutate(payload)
			}
			res, err := tmplRunJob(ops, tc.queue, payload)
			if err != nil {
				t.Fatalf("Execute returned a transport error: %v", err)
			}
			if res.Status != JobStatusFailure {
				t.Fatalf("status = %q, want failure", res.Status)
			}
			if ops.called {
				t.Fatalf("ops.Run was called on a refusal path; it must not be")
			}
			if got := jobErrText(res); !strings.Contains(got, tc.wantErr) {
				t.Fatalf("error %q does not contain %q", got, tc.wantErr)
			}
		})
	}
}

// rehashRunner swaps the runner and re-derives the matching content_hash so a
// runner-kind test fails on the KIND check, not the earlier hash check.
func rehashRunner(t *testing.T, p map[string]any, runner string) {
	t.Helper()
	p["runner"] = runner
	hash, err := jobs.ComputeTemplateManifestHash(tmplKey, tmplVersion,
		json.RawMessage(tmplInSchema), json.RawMessage(tmplOutSchema), json.RawMessage(runner))
	if err != nil {
		t.Fatalf("rehash: %v", err)
	}
	p["content_hash"] = hash
}

func TestRunJobTemplate_UnknownHandlerIsTerminal(t *testing.T) {
	ops := &fakeTemplateOps{err: ErrTemplateUnknownHandler}
	res, err := tmplRunJob(ops, tmplPerNodeQueue, tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !ops.called {
		t.Fatal("ops.Run should be called once the request is verified")
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("status = %q, want failure (terminal)", res.Status)
	}
}

func TestRunJobTemplate_TransientRetries(t *testing.T) {
	ops := &fakeTemplateOps{err: ErrTemplateTransient}
	res, err := tmplRunJob(ops, tmplPerNodeQueue, tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != JobStatusRetry {
		t.Fatalf("status = %q, want retry", res.Status)
	}
}

func TestRunJobTemplate_HappyPath(t *testing.T) {
	want := &TemplateRunResult{
		Outputs: []TemplateOutput{
			{Path: "templates/audio-mix/job-1/out/mix.wav", SHA256: "sha256:abc", Bytes: 12345},
		},
		DurationMs:  5000,
		InputHashes: []string{"sha256:def"},
	}
	ops := &fakeTemplateOps{result: want}
	res, err := tmplRunJob(ops, tmplPerNodeQueue, tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema))
	if err != nil {
		t.Fatal(err)
	}
	if !ops.called {
		t.Fatal("ops.Run was not called on the verified happy path")
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %q, want success", res.Status)
	}
	if _, ok := res.Output["outputs"]; !ok {
		t.Errorf("Output missing outputs: %v", res.Output)
	}
	if dm, ok := res.Output["duration_ms"].(float64); !ok || int64(dm) != 5000 {
		t.Errorf("Output duration_ms = %v, want 5000", res.Output["duration_ms"])
	}
	if _, ok := res.Output["input_hashes"]; !ok {
		t.Errorf("Output missing input_hashes: %v", res.Output)
	}
}

// TestRunJobTemplate_ContentHashAcceptsSha256Prefix pins that a "sha256:"-
// prefixed content_hash still matches the bare-hex recomputation.
func TestRunJobTemplate_ContentHashAcceptsSha256Prefix(t *testing.T) {
	ops := &fakeTemplateOps{result: &TemplateRunResult{}}
	payload := tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema)
	payload["content_hash"] = "sha256:" + payload["content_hash"].(string)
	res, err := tmplRunJob(ops, tmplPerNodeQueue, payload)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("status = %q, want success (prefixed hash should match)", res.Status)
	}
}

func jobErrText(res *JobResult) string {
	if res == nil {
		return ""
	}
	if e, ok := res.Output["error"].(string); ok {
		return e
	}
	if res.Error != nil {
		return res.Error.Error()
	}
	return ""
}
