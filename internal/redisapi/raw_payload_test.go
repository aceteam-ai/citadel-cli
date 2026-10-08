package redisapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParseStreamMessagePreservesRawPayload(t *testing.T) {
	for _, payload := range []string{
		" \n { \"command\" : \"fixture\", \"z\": 2, \"a\": 1 }\t ",
		`{"command":"fixture","integer":9007199254740993}`,
		`{"command":"你好 🌍 café","escaped":"\u00e9"}`,
		`null`,
	} {
		job, err := ParseStreamMessage(StreamMessage{Data: StreamMessageData{Payload: payload}})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(job.RawPayload, []byte(payload)) || sha256.Sum256(job.RawPayload) != sha256.Sum256([]byte(payload)) {
			t.Fatal("authoritative payload bytes or digest changed")
		}
	}
}

func TestParseStreamMessageRawPayloadBoundaries(t *testing.T) {
	job, err := ParseStreamMessage(StreamMessage{})
	if err != nil || job.RawPayload != nil || job.Payload != nil {
		t.Fatal("missing input acquired fabricated bytes")
	}
	for _, payload := range []string{" \t\n", "{invalid"} {
		if _, err := ParseStreamMessage(StreamMessage{Data: StreamMessageData{Payload: payload}}); err == nil {
			t.Fatal("invalid payload no longer returns its existing parse error")
		}
	}
}

func TestJobMarshalExcludesRawPayload(t *testing.T) {
	const sentinel = "RAW_ONLY_PAYLOAD_SENTINEL"
	job := Job{JobID: "fixture", Payload: map[string]any{"command": "fixture"}, RawPayload: []byte(sentinel)}
	body, err := json.Marshal(&job)
	if err != nil {
		t.Fatal(err)
	}
	job.RawPayload = nil
	baseline, err := json.Marshal(&job)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, baseline) || bytes.Contains(body, []byte(sentinel)) || bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString([]byte(sentinel)))) || bytes.Contains(body, []byte("RawPayload")) || bytes.Contains(body, []byte("raw_payload")) {
		t.Fatal("internal payload metadata leaked into serialized job")
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 || fields["job_id"] != "fixture" {
		t.Fatal("public job representation changed")
	}
}
