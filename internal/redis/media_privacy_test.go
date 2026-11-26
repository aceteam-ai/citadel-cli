package redis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestMediaIntakeSealsPrivateFieldsWithoutChangingExecution(t *testing.T) {
	const token = "synthetic-redis-media-token"
	for _, inputs := range []string{`null`, `false`, `"wrong type"`, `{"capability":"` + token + `"}`} {
		raw := ` {"type":"MEDIA_GENERATE","inputs":` + inputs + `,"prompt":"` + token + `","n":1e+03} `
		job, err := (&Client{}).parseMessage(goredis.XMessage{ID: "1-0", Values: map[string]any{
			"jobId": "synthetic-job", "payload": raw, "rayId": "synthetic-ray", "enqueuedAt": "synthetic-time",
		}})
		if err != nil || job.Type != "MEDIA_GENERATE" || job.PrivateMediaPayload() == nil {
			t.Fatalf("private fallback-type intake failed: %v", err)
		}
		if job.RawPayload != nil || job.RawData != nil || len(job.Payload) != 1 || job.EnqueuedAt != "synthetic-time" {
			t.Fatal("observable fields retained original payload or lost enqueue identity")
		}
		private := job.PrivateMediaPayload()
		payload, err := private.ExecutionPayload()
		if err != nil || payload["prompt"] != token || payload["n"] != float64(1000) {
			t.Fatal("execution projection changed")
		}
		if !bytes.Equal(private.OriginalPayload(), []byte(raw)) {
			t.Fatal("wire bytes changed")
		}
		for _, value := range []any{job, *job, struct{ job Job }{*job}, struct{ job *Job }{job}} {
			encoded, err := json.Marshal(value)
			if err != nil || strings.Contains(string(encoded), token) {
				t.Fatal("parsed job JSON retained private input")
			}
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
				if strings.Contains(fmt.Sprintf(verb, value), token) {
					t.Fatalf("parsed job reflected private input through %s", verb)
				}
			}
		}
		job.sealMedia()
		if !bytes.Equal(job.PrivateMediaPayload().OriginalPayload(), []byte(raw)) {
			t.Fatal("resealing replaced original execution with diagnostic marker")
		}
	}
}

func TestMediaIntakeBytePayloadRemainsRawOnly(t *testing.T) {
	raw := []byte(`{"inputs":{"capability":"synthetic-byte-token"}}`)
	job, err := (&Client{}).parseMessage(goredis.XMessage{ID: "1-0", Values: map[string]any{"type": "MEDIA_GENERATE", "payload": raw}})
	if err != nil || job.PrivateMediaPayload() == nil || job.RawPayload != nil {
		t.Fatal("raw-only intake was not sealed")
	}
	if _, err := job.PrivateMediaPayload().ExecutionPayload(); err == nil {
		t.Fatal("byte-only source gained executable fields")
	}
	if !bytes.Equal(job.PrivateMediaPayload().OriginalPayload(), raw) {
		t.Fatal("byte-only source lost exact bytes")
	}
}

func TestMediaIntakeLegacyTextKeepsRetention(t *testing.T) {
	raw := `{"prompt":"legacy-text","n":1e+03}`
	job, err := (&Client{}).parseMessage(goredis.XMessage{Values: map[string]any{"type": "MEDIA_GENERATE", "payload": raw}})
	if err != nil || job.PrivateMediaPayload() != nil || job.Payload["prompt"] != "legacy-text" || string(job.RawPayload) != raw || job.RawData["payload"] != raw {
		t.Fatal("legacy text retention changed")
	}
	encoded, _ := json.Marshal(job)
	if !strings.Contains(string(encoded), "legacy-text") {
		t.Fatal("legacy diagnostics changed")
	}
}
