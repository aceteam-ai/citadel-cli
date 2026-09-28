package worker

import (
	"encoding/json"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"strings"
	"testing"
)

func TestReviewMalformedParamsRefusedBeforeOps(t *testing.T) {
	p := tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema)
	p["params"] = `{"gain":`
	ops := &fakeTemplateOps{result: &TemplateRunResult{}}
	res, err := tmplRunJob(ops, tmplPerNodeQueue, p)
	if err != nil {
		t.Fatal(err)
	}
	if ops.called {
		t.Fatalf("malformed params reached ops, status=%s", res.Status)
	}
}

func TestReviewRunnerReorderingCannotChangeHandler(t *testing.T) {
	a := json.RawMessage(`{"kind":"builtin","handler":"safe","Handler":"evil"}`)
	b := json.RawMessage(`{"kind":"builtin","Handler":"evil","handler":"safe"}`)
	ha, _ := jobs.ComputeTemplateManifestHash("k", 1, json.RawMessage(`{}`), json.RawMessage(`{}`), a)
	hb, _ := jobs.ComputeTemplateManifestHash("k", 1, json.RawMessage(`{}`), json.RawMessage(`{}`), b)
	ra, ea := parseRunnerDescriptor(a)
	rb, eb := parseRunnerDescriptor(b)
	if ha != hb || ha != "5effa864e181c3ee4e0520b65fbc1f321767b0c2c8cb800afccf3d76228ce9b9" {
		t.Fatalf("collision fixture changed: %s %s", ha, hb)
	}
	if ea == nil || eb == nil {
		t.Fatalf("ambiguous runners accepted: %v %v (%v %v)", ra, rb, ea, eb)
	}
	for _, runner := range []string{string(a), string(b)} {
		ops := &fakeTemplateOps{result: &TemplateRunResult{}}
		res, err := tmplRunJob(ops, tmplPerNodeQueue, tmplPayload(t, runner, tmplInSchema, tmplOutSchema))
		if err != nil || res.Status != JobStatusFailure || ops.called {
			t.Fatalf("ambiguous runner reached ops: %v %v %v", res, err, ops.called)
		}
	}
}

func TestRunJobTemplate_InputContractRefusedBeforeOps(t *testing.T) {
	for _, tc := range []struct{ name, field, value, errText string }{
		{"array_params", "params", `[]`, "params must be an object"},
		{"null_params", "params", `null`, "params must be an object"},
		{"scalar_params", "params", `1`, "params must be an object"},
		{"wrong_property_type", "params", `{"gain":"loud"}`, "violate input_schema"},
		{"duplicate_params", "params", `{"gain":1,"gain":2}`, "duplicate JSON key"},
		{"trailing_params", "params", `{} {}`, "invalid JSON"},
		{"malformed_files", "input_files", `[{`, "input_files"},
		{"null_files", "input_files", `null`, "must be an array"},
		{"foreign_node", "input_files", `[{"path":"node:foreign/a.txt","node_id":"foreign","node_path":"a.txt"}]`, "executing node"},
		{"missing_identity", "input_files", `[{"path":"a.txt","node_path":"a.txt"}]`, "executing node"},
		{"missing_node_path", "input_files", `[{"path":"a.txt","node_id":"1008"}]`, "node_path"},
		{"disagreeing_path", "input_files", `[{"path":"node:foreign/a.txt","node_id":"1008","node_path":"a.txt"}]`, "disagrees"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema)
			p[tc.field] = tc.value
			ops := &fakeTemplateOps{result: &TemplateRunResult{}}
			res, err := tmplRunJob(ops, tmplPerNodeQueue, p)
			if err != nil || res.Status != JobStatusFailure || ops.called || !strings.Contains(jobErrText(res), tc.errText) {
				t.Fatalf("res=%v err=%v ops.called=%v", res, err, ops.called)
			}
		})
	}
}

func TestRunJobTemplate_RequiredAndConstrainedParams(t *testing.T) {
	schema := `{"type":"object","properties":{"gain":{"type":"number","minimum":0,"maximum":2}},"required":["gain"],"additionalProperties":false}`
	for _, params := range []string{`{}`, `{"gain":-1}`, `{"gain":3}`, `{"gain":1,"extra":true}`} {
		p := tmplPayload(t, tmplBuiltinRunner, schema, tmplOutSchema)
		p["params"] = params
		ops := &fakeTemplateOps{result: &TemplateRunResult{}}
		res, err := tmplRunJob(ops, tmplPerNodeQueue, p)
		if err != nil || res.Status != JobStatusFailure || ops.called {
			t.Fatalf("invalid params %s reached ops: %v %v", params, res, err)
		}
	}
}

func TestRunJobTemplate_LocalNodeQueueBinding(t *testing.T) {
	ops := &fakeTemplateOps{result: &TemplateRunResult{}}
	res, err := tmplRunJob(ops, "jobs:v1:shell:org_test:node:other", tmplPayload(t, tmplBuiltinRunner, tmplInSchema, tmplOutSchema))
	if err != nil || res.Status != JobStatusFailure || ops.called {
		t.Fatalf("foreign queue reached ops: %v %v", res, err)
	}
}
