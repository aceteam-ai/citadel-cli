package redisapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMediaAPIIntakeSealsExecutionAndSourceMetadata(t *testing.T) {
	const token = "synthetic-api-media-token"
	raw := " {\"type\":\"MEDIA_GENERATE\",\"inpu\\u0074s\":{\"capability\":\"" + token + "\"},\"n\":1e+03} \n"
	job, err := ParseStreamMessage(StreamMessage{ID: "1-0", Data: StreamMessageData{JobID: "synthetic", Payload: raw, RayID: "ray-fixture", EnqueuedAt: "time-fixture"}})
	if err != nil || job.PrivateMediaPayload() == nil || job.Type != "MEDIA_GENERATE" {
		t.Fatalf("private intake failed: %v", err)
	}
	if job.RawPayload != nil || job.RawData != nil {
		t.Fatal("observable raw input retained")
	}
	private := job.PrivateMediaPayload()
	if string(private.OriginalPayload()) != raw {
		t.Fatal("exact bytes changed")
	}
	payload, err := private.ExecutionPayload()
	if err != nil || payload["n"] != float64(1000) || payload["inputs"].(map[string]any)["capability"] != token {
		t.Fatal("execution authority changed")
	}
	data, _ := private.OriginalData()
	if data["rayId"] != "ray-fixture" || data["enqueuedAt"] != "time-fixture" {
		t.Fatal("source identity changed")
	}
	for _, value := range []any{job, *job, struct{ job *Job }{job}, struct{ job Job }{*job}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
			if strings.Contains(fmt.Sprintf(verb, value), token) {
				t.Fatalf("parsed API job reflected credential through %s", verb)
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), token) {
			t.Fatal("parsed API job JSON retained credential")
		}
	}
}

func TestMediaAPIIntakeLegacyAndParseRefusal(t *testing.T) {
	raw := `{"prompt":"legacy-text"}`
	job, err := ParseStreamMessage(StreamMessage{Data: StreamMessageData{Type: "MEDIA_GENERATE", Payload: raw}})
	if err != nil || job.PrivateMediaPayload() != nil || string(job.RawPayload) != raw || job.Payload["prompt"] != "legacy-text" {
		t.Fatal("legacy text retention changed")
	}
	job, err = ParseStreamMessage(StreamMessage{Data: StreamMessageData{Type: "MEDIA_GENERATE", Payload: `{"inputs":1e9999}`}})
	if err == nil || job != nil || strings.Contains(fmt.Sprintf("%#v", struct{ err error }{err}), "1e9999") || strings.Contains(err.Error(), "1e9999") {
		t.Fatal("failed media decoding retained original value")
	}
}
