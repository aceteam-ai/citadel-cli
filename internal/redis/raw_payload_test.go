package redis

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestParseMessagePreservesRawPayload(t *testing.T) {
	for _, payload := range []string{
		" \n { \"command\" : \"fixture\", \"z\": 2, \"a\": 1 }\t ",
		`{"command":"fixture","integer":9007199254740993}`,
		`{"command":"你好 🌍 café","escaped":"\u00e9"}`,
		`null`,
	} {
		job, err := NewClient(ClientConfig{}).parseMessage(goredis.XMessage{ID: "1-0", Values: map[string]any{"payload": payload}})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(job.RawPayload, []byte(payload)) || sha256.Sum256(job.RawPayload) != sha256.Sum256([]byte(payload)) {
			t.Fatal("authoritative payload bytes or digest changed")
		}
		var expected map[string]any
		if err := json.Unmarshal([]byte(payload), &expected); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(job.Payload)
		want, _ := json.Marshal(expected)
		if !bytes.Equal(got, want) {
			t.Fatal("legacy decoded payload changed")
		}
	}
}

func TestParseMessageRawPayloadBoundaries(t *testing.T) {
	client := NewClient(ClientConfig{})
	for _, fields := range []map[string]any{nil, {"payload": nil}, {"payload": map[string]any{"command": "fixture"}}} {
		job, err := client.parseMessage(goredis.XMessage{Values: fields})
		if err != nil || job.RawPayload != nil || job.Payload != nil {
			t.Fatalf("missing or unsupported input acquired fabricated bytes: err=%v", err)
		}
	}
	for _, payload := range []string{"", " \t\n", "{invalid"} {
		if _, err := client.parseMessage(goredis.XMessage{Values: map[string]any{"payload": payload}}); err == nil {
			t.Fatal("invalid string payload no longer returns its existing parse error")
		}
	}
	raw := []byte(`{"command":"fixture"}`)
	job, err := client.parseMessage(goredis.XMessage{Values: map[string]any{"payload": raw}})
	if err != nil || !bytes.Equal(job.RawPayload, raw) || job.Payload != nil {
		t.Fatal("byte-valued metadata changed legacy string-only decoding")
	}
	raw[0] = 'x'
	if job.RawPayload[0] != '{' {
		t.Fatal("raw payload aliases the caller's mutable byte slice")
	}
}

func TestRedisJobMarshalExcludesRawPayload(t *testing.T) {
	const sentinel = "RAW_ONLY_PAYLOAD_SENTINEL"
	job := Job{JobID: "fixture", RawPayload: []byte(sentinel)}
	body, err := json.Marshal(&job)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	job.RawPayload = nil
	baseline, err := json.Marshal(&job)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 || fields["JobID"] != "fixture" || !bytes.Equal(body, baseline) || bytes.Contains(body, []byte(base64.StdEncoding.EncodeToString([]byte(sentinel)))) {
		t.Fatal("internal payload metadata changed serialized Redis job")
	}
}
