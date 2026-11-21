package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
)

func mediaAttributionUsageFixture(t *testing.T, identity any) (usage.UsageRecord, *Job, []byte) {
	t.Helper()
	payload := map[string]any{
		"inputs":            map[string]any{"capability": mediaPrivacyToken},
		"flow_run_id":       identity,
		"flow_node_id":      identity,
		"delegated_node_id": identity,
		"node_uid":          identity,
		"receipt_ids":       []any{"fixture-receipt"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: "fixture-attribution", Type: JobTypeMediaGenerate, Payload: payload, RawPayload: raw}
	job.sealMedia()
	source := NewMockJobSource("fixture", nil)
	runner := NewRunner(source, []JobHandler{mediaPrivacyFailureHandler{status: JobStatusFailure}}, RunnerConfig{
		ActivityFn: func(string, string) {},
	})
	var records []usage.UsageRecord
	runner.jobRecordFn = func(record usage.UsageRecord) { records = append(records, record) }
	if runner.executeJob(context.Background(), job, &MockStreamWriter{}, time.Now(), false, 0) || len(records) != 1 {
		t.Fatal("fixture did not reach the failed-job usage hook exactly once")
	}
	return records[0], job, raw
}

func TestMediaUsageAttributionRejectsNonStringIdentity(t *testing.T) {
	for _, identity := range []any{
		map[string]any{"capability": mediaPrivacyToken},
		[]any{mediaPrivacyToken}, float64(123), true, nil,
	} {
		record, job, raw := mediaAttributionUsageFixture(t, identity)
		if record.FlowRunID != "" || record.FlowNodeID != "" || record.DelegatedNodeID != "" || record.NodeUID != "" {
			t.Errorf("private non-string identity entered the retained usage hook: type=%T", identity)
		}
		if strings.Contains(fmt.Sprintf("%#v", record), mediaPrivacyToken) {
			t.Error("private nested input entered retained usage attribution")
		}
		payload, err := job.executionPayload()
		if err != nil || rawOrPayload(job) != string(raw) {
			t.Fatal("diagnostic projection changed original execution types or receipt bytes")
		}
		for _, key := range []string{"flow_run_id", "flow_node_id", "delegated_node_id", "node_uid"} {
			if !reflect.DeepEqual(payload[key], identity) {
				t.Fatal("diagnostic projection changed an original identity value")
			}
		}
		if !reflect.DeepEqual(record.ReceiptIDs, []string{"fixture-receipt"}) {
			t.Fatal("identity diagnostic policy changed existing receipt identifiers")
		}
	}
}

func TestMediaUsageAttributionPreservesStringIdentity(t *testing.T) {
	record, job, raw := mediaAttributionUsageFixture(t, "fixture-identity")
	if record.FlowRunID != "fixture-identity" || record.FlowNodeID != "fixture-identity" || record.DelegatedNodeID != "fixture-identity" || record.NodeUID != "fixture-identity" {
		t.Fatal("private media lost existing benign string attribution")
	}
	if rawOrPayload(job) != string(raw) || !reflect.DeepEqual(record.ReceiptIDs, []string{"fixture-receipt"}) {
		t.Fatal("string attribution policy changed receipt semantics")
	}
	// Ordinary jobs retain their existing generic coercion policy.
	legacy := &Job{Type: JobTypeMediaGenerate, Payload: map[string]any{"flow_run_id": map[string]any{"legacy": "value"}}}
	legacyRecord := buildUsageRecord(legacy, "failed", time.Now(), time.Now(), nil, nil)
	if legacyRecord.FlowRunID != "map[legacy:value]" {
		t.Fatal("private media policy changed ordinary-job attribution coercion")
	}
}

func TestMediaWSConfirmationRetentionBoundary(t *testing.T) {
	const secret = "synthetic-ws-confirmation.secret"
	for _, detail := range []string{
		secret, `{"inputs":{"capability":"` + secret + `"}}`, strings.Repeat(secret, 3000), "",
	} {
		var logs []string
		source := NewWSSource(WSSourceConfig{DebugFunc: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		}})
		for _, kind := range []string{"consuming", "acked"} {
			wire, err := json.Marshal(redisapi.WSMessage{Type: kind, Queues: []string{"jobs:v1:fixture", detail}, MessageID: detail})
			if err != nil {
				t.Fatal(err)
			}
			var decoded redisapi.WSMessage
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			// Same callbacks registered by Connect; no connection or listener.
			if kind == "consuming" {
				source.onServerConsuming(decoded)
			} else {
				source.onServerAcked(decoded)
			}
			after, err := json.Marshal(decoded)
			if err != nil || !bytes.Equal(after, wire) {
				t.Fatal("confirmation diagnostics changed the original wire carrier")
			}
		}
		if len(logs) != 2 || !strings.Contains(logs[0], "confirmed consuming") || !strings.Contains(logs[1], "confirmed ack") {
			t.Fatal("server confirmations stopped being observable")
		}
		if strings.Contains(strings.Join(logs, "\n"), secret) {
			t.Error("pre-kind server confirmation retained private input detail")
		}
	}
}
