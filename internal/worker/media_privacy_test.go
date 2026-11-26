package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

const mediaPrivacyToken = "synthetic-worker-media-token"

func mediaPrivacyRaw() string {
	return ` {"inputs":{"capability":"` + mediaPrivacyToken + `"},"target_node":"fixture-node","targetGpu":1,"timeout_ms":4321,"org_id":"fixture-org","flow_run_id":"fixture-flow","flow_node_id":"fixture-flow-node","delegated_node_id":"fixture-delegate","node_uid":"fixture-uid","receipt_ids":["fixture-receipt"],"model":"` + mediaPrivacyToken + `","backend":"` + mediaPrivacyToken + `","n":1e+03,"escaped":"\u0061"} `
}

func mediaPrivacyJob(t *testing.T) *Job {
	t.Helper()
	raw := mediaPrivacyRaw()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "fixture-job", Type: "MEDIA_GENERATE", MessageID: "1-0", Payload: payload, RawPayload: []byte(raw)}
	job.sealMedia()
	return job
}

func assertMediaJobPrivate(t *testing.T, job *Job) {
	t.Helper()
	if job.media == nil || job.RawPayload != nil || len(job.Payload) != 1 {
		t.Fatal("worker retained observable execution fields")
	}
	for _, value := range []any{job, *job, struct{ job Job }{*job}, struct{ job *Job }{job}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
			if strings.Contains(fmt.Sprintf(verb, value), mediaPrivacyToken) {
				t.Fatalf("worker credential reflected through %s", verb)
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), mediaPrivacyToken) {
			t.Fatal("worker JSON retained private input")
		}
	}
}

func TestMediaWorkerConversionsPreserveExecutionAndTrace(t *testing.T) {
	raw := mediaPrivacyRaw()
	var payload map[string]any
	_ = json.Unmarshal([]byte(raw), &payload)
	redisJob := (&RedisSource{}).convertJob(&redisclient.Job{
		JobID: "fixture-job", Type: "MEDIA_GENERATE", MessageID: "1-0", Payload: payload,
		RawPayload: []byte(raw), RawData: map[string]any{"rayId": "fixture-ray", "payload": raw},
	})
	apiJob, err := redisapi.ParseStreamMessage(redisapi.StreamMessage{ID: "1-0", Data: redisapi.StreamMessageData{
		JobID: "fixture-job", Type: "MEDIA_GENERATE", Payload: raw, RayID: "fixture-ray",
	}})
	if err != nil {
		t.Fatal(err)
	}
	apiWorker := (&APISource{}).convertJob(apiJob)
	wsWorker, err := NewWSSource(WSSourceConfig{}).convertWSJob(redisapi.WSMessage{
		ID: "1-0", Queue: "jobs:v1:fixture", Data: map[string]string{
			"jobId": "fixture-job", "type": "MEDIA_GENERATE", "payload": raw, "rayId": "fixture-ray",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range []*Job{redisJob, apiWorker, wsWorker} {
		assertMediaJobPrivate(t, job)
		if job.ID != "fixture-job" || job.MessageID != "1-0" || job.RayID != "fixture-ray" {
			t.Fatal("source identity lost")
		}
		if got := rawOrPayload(job); got != raw {
			t.Fatal("receipt digest substituted normalized/sanitized bytes")
		}
		projected, err := job.executionPayload()
		if err != nil || projected["target_node"] != "fixture-node" || projected["n"] != float64(1000) {
			t.Fatal("source conversion lost execution authority")
		}
		projected["inputs"].(map[string]any)["capability"] = "caller-mutation"
		again, _ := job.executionPayload()
		if again["inputs"].(map[string]any)["capability"] != mediaPrivacyToken {
			t.Fatal("worker projection was not owned")
		}
	}
	if wsWorker.SourceQueue != "jobs:v1:fixture" {
		t.Fatal("WS queue identity lost")
	}
}

func TestMediaWorkerControlAndReceiptProjections(t *testing.T) {
	job := mediaPrivacyJob(t)
	if timeout, ok := jobExecTimeout(job); !ok || timeout != 4321*time.Millisecond {
		t.Fatal("sealing lost explicit execution deadline")
	}
	if gpu, ok := job.executionField("targetGpu"); !ok || gpu != float64(1) {
		t.Fatal("sealing lost requested GPU")
	}
	job.setGPUIndex(3)
	job.Payload["_gpuIndex"] = 99 // Diagnostic/public maps cannot alter execution.
	projected, err := job.executionPayload()
	if err != nil || projected["_gpuIndex"] != 3 {
		t.Fatal("trusted GPU selection was not projected")
	}
	delete(job.Payload, "_gpuIndex")
	job.sealMedia()
	if rawOrPayload(job) != mediaPrivacyRaw() {
		t.Fatal("GPU selection/resealing changed original receipt input")
	}
	runner := NewRunner(NewMockJobSource("fixture", nil), nil, RunnerConfig{ActivityFn: func(string, string) {}})
	if runner.newRunnerWorkReceiptConfig(job).OrgID != "fixture-org" {
		t.Fatal("sealing lost receipt org attribution")
	}
	delete(job.Payload, "inputs") // A mutable diagnostic map cannot declassify it.
	record := buildUsageRecord(job, "failed", time.Now(), time.Now(), nil, errors.New(mediaPrivacyToken))
	if record.FlowRunID != "fixture-flow" || record.FlowNodeID != "fixture-flow-node" || record.DelegatedNodeID != "fixture-delegate" || record.NodeUID != "fixture-uid" || len(record.ReceiptIDs) != 1 || record.ReceiptIDs[0] != "fixture-receipt" {
		t.Fatal("sealing lost typed flow attribution")
	}
	if record.Model != "" || record.Backend != "" || strings.Contains(record.ErrorMessage, mediaPrivacyToken) {
		t.Fatal("unverified request identity/error entered retained usage")
	}
	result := &JobResult{Output: map[string]any{
		"usage":  map[string]any{"origin": mediaPrivacyToken, "org_id": mediaPrivacyToken},
		"output": `{"usage":{"origin":"` + mediaPrivacyToken + `"}}`,
	}}
	record = buildUsageRecord(job, "failed", time.Now(), time.Now(), result, errors.New(mediaPrivacyToken))
	if strings.Contains(fmt.Sprintf("%#v", record), mediaPrivacyToken) {
		t.Fatal("unreviewed arbitrary handler usage entered retained record")
	}
	job.sealMedia()
	assertMediaJobPrivate(t, job)
}

func TestMediaWorkerClaimPreservesRoutingAndRefusesRawOnly(t *testing.T) {
	for _, node := range []string{"fixture-node", "another-node", ""} {
		source := NewMockJobSource("fixture", nil)
		runner := NewRunner(source, nil, RunnerConfig{NodeID: node, ActivityFn: func(string, string) {}})
		proceed, _, _ := runner.claimJob(context.Background(), mediaPrivacyJob(t))
		if proceed != (node == "fixture-node") {
			t.Fatal("sealing bypassed target-node routing")
		}
		if !proceed && len(source.AckedJobs()) != 1 {
			t.Fatal("wrong-node delivery acknowledgement changed")
		}
	}
	source := NewMockJobSource("fixture", nil)
	runner := NewRunner(source, nil, RunnerConfig{ActivityFn: func(string, string) {}})
	job := &Job{ID: "raw-only", Type: "MEDIA_GENERATE", RawPayload: []byte(`{"inputs":"` + mediaPrivacyToken + `"}`)}
	proceed, _, _ := runner.claimJob(context.Background(), job)
	if proceed || len(source.FailedJobs()) != 1 {
		t.Fatal("raw-only capture acquired execution authority")
	}
	assertMediaJobPrivate(t, job)
}

type mediaPrivacyLegacyHandler struct {
	t       *testing.T
	fail    bool
	invoked bool
}

func (h *mediaPrivacyLegacyHandler) Execute(ctx jobs.JobContext, job *nexus.Job) ([]byte, error) {
	h.invoked = true
	if !strings.Contains(job.Payload["inputs"], mediaPrivacyToken) || job.Payload["target_node"] != "fixture-node" || job.Payload["_gpuIndex"] != "2" {
		h.t.Fatal("legacy handler received diagnostic marker instead of execution data")
	}
	ctx.Log(mediaPrivacyToken, "handler diagnostic: %s", mediaPrivacyToken)
	if ctx.Progress != nil {
		h.t.Fatal("unreviewed arbitrary progress fields enabled for private media")
	}
	if h.fail {
		return []byte(mediaPrivacyToken), errors.New(mediaPrivacyToken)
	}
	return []byte(`{"status":"synthetic-success"}`), nil
}

func TestMediaWorkerLegacyProjectionAndFailurePrivacy(t *testing.T) {
	for _, fail := range []bool{false, true} {
		handler := &mediaPrivacyLegacyHandler{t: t, fail: fail}
		adapter := NewLegacyHandlerAdapter("MEDIA_GENERATE", handler)
		var logs []string
		adapter.SetLogFn(func(level string, text string) { logs = append(logs, level+text) })
		job := mediaPrivacyJob(t)
		job.setGPUIndex(2)
		result, err := adapter.Execute(context.Background(), job, &MockStreamWriter{})
		if !handler.invoked || (err != nil) != fail || result == nil {
			t.Fatal("legacy execution semantics changed")
		}
		if strings.Contains(strings.Join(logs, "\n"), mediaPrivacyToken) || strings.Contains(fmt.Sprintf("%#v", result.Output), mediaPrivacyToken) {
			t.Fatal("handler output/error/log retained private input")
		}
		if err != nil && strings.Contains(fmt.Sprintf("%#v", struct{ err error }{err}), mediaPrivacyToken) {
			t.Fatal("original handler error reflected private input")
		}
		assertMediaJobPrivate(t, job)
	}
}

type mediaPrivacyFailureHandler struct{ status JobStatus }

func (h mediaPrivacyFailureHandler) CanHandle(kind string) bool { return kind == "MEDIA_GENERATE" }
func (h mediaPrivacyFailureHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	return &JobResult{
		Status: h.status,
		Error:  errors.New(mediaPrivacyToken),
		Output: map[string]any{"usage": map[string]any{"origin": mediaPrivacyToken}},
	}, nil
}

func TestMediaWorkerRetainedFailureRetryAndTerminalPaths(t *testing.T) {
	for _, status := range []JobStatus{JobStatusFailure, JobStatusRetry, JobStatusTerminalFailure} {
		t.Run(string(status), func(t *testing.T) {
			var retained []string
			source := NewMockJobSource("fixture", nil)
			runner := NewRunner(source, []JobHandler{mediaPrivacyFailureHandler{status: status}}, RunnerConfig{
				ActivityFn: func(_ string, message string) { retained = append(retained, message) },
			})
			runner.jobRecordFn = func(record usage.UsageRecord) { retained = append(retained, fmt.Sprintf("%#v", record)) }
			stream := &MockStreamWriter{}
			runner.executeJob(context.Background(), mediaPrivacyJob(t), stream, time.Now(), false, 0)
			if stream.erroredErr != nil {
				retained = append(retained, fmt.Sprintf("%#v", struct{ err error }{stream.erroredErr}), stream.erroredErr.Error())
			}
			retained = append(retained, fmt.Sprintf("%#v", source.FailedData()))
			if strings.Contains(strings.Join(retained, "\n"), mediaPrivacyToken) {
				t.Fatal("retained terminal/retry/log/usage path contains input credential")
			}
		})
	}
}

func TestMediaWorkerReceiptFallbackIsOriginalExecutionNotMarker(t *testing.T) {
	for _, raw := range [][]byte{nil, {}} {
		job := &Job{Type: "MEDIA_GENERATE", Payload: map[string]any{"inputs": nil, "n": float64(1000)}, RawPayload: raw}
		job.sealMedia()
		original, ok := rawOrPayload(job).(map[string]any)
		if !ok || original["inputs"] != nil || original["n"] != float64(1000) {
			t.Fatal("nil/empty raw fallback substituted diagnostic marker")
		}
	}
}
