package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
)

func TestWorkerSourcesPreserveRawPayload(t *testing.T) {
	for _, payload := range []string{
		" \n { \"command\" : \"fixture\", \"z\": 2, \"a\": 1 }\t ",
		`{"command":"fixture","integer":9007199254740993}`,
		`{"command":"你好 🌍 café","escaped":"\u00e9"}`,
		`null`,
	} {
		for _, transport := range []string{"redis", "http", "websocket"} {
			t.Run(transport+"/"+payload, func(t *testing.T) {
				var job *Job
				switch transport {
				case "redis":
					mr := miniredis.RunT(t)
					source := NewRedisSource(RedisSourceConfig{URL: "redis://" + mr.Addr(), QueueName: "jobs:v1:raw-fixture", BlockMs: 10, LogFn: func(string, string) {}})
					if err := source.Connect(context.Background()); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = source.Close() })
					raw := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
					t.Cleanup(func() { _ = raw.Close() })
					if err := raw.XAdd(context.Background(), &goredis.XAddArgs{Stream: "jobs:v1:raw-fixture", Values: map[string]any{"jobId": "fixture", "type": JobTypeShellCommand, "payload": payload}}).Err(); err != nil {
						t.Fatal(err)
					}
					var err error
					job, err = source.Next(context.Background())
					if err != nil || job == nil {
						t.Fatalf("read local fixture: err=%v", err)
					}
				case "http":
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						if req.URL.Path != "/api/fabric/redis/jobs/consume" {
							http.NotFound(w, req)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(redisapi.ConsumeResponse{Messages: []redisapi.StreamMessage{{ID: "1-0", Data: redisapi.StreamMessageData{JobID: "fixture", Type: JobTypeShellCommand, Payload: payload}}}})
					}))
					t.Cleanup(server.Close)
					client := redisapi.NewClient(redisapi.ClientConfig{BaseURL: server.URL, Token: "local-fixture"})
					t.Cleanup(func() { _ = client.Close() })
					parsed, err := client.ConsumeJob(context.Background(), redisapi.ConsumeRequest{Queue: "fixture", Group: "fixture", Consumer: "fixture", Count: 1, BlockMs: 1})
					if err != nil || parsed == nil {
						t.Fatalf("parse HTTP fixture: err=%v", err)
					}
					job = NewAPISource(APISourceConfig{}).convertJob(parsed)
				case "websocket":
					var err error
					job, err = NewWSSource(WSSourceConfig{}).convertWSJob(redisapi.WSMessage{Type: "job", ID: "1-0", Data: map[string]string{"jobId": "fixture", "type": JobTypeShellCommand, "payload": payload}})
					if err != nil {
						t.Fatal(err)
					}
				}
				if !bytes.Equal(job.RawPayload, []byte(payload)) || sha256.Sum256(job.RawPayload) != sha256.Sum256([]byte(payload)) {
					t.Fatal("original queue payload bytes or digest changed")
				}
				var expected map[string]any
				if err := json.Unmarshal([]byte(payload), &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(job.Payload, expected) || job.ID != "fixture" || job.Type != JobTypeShellCommand {
					t.Fatal("decoded handler input or routing metadata changed")
				}
				if job.Payload != nil {
					job.Payload["fixture_mutation"] = true
					if !bytes.Equal(job.RawPayload, []byte(payload)) {
						t.Fatal("handler mutation changed authoritative bytes")
					}
				}
			})
		}
	}
}

func TestWorkerRawPayloadAbsentAndOwned(t *testing.T) {
	input := []byte(`{"command":"fixture"}`)
	for _, source := range []string{"redis", "http"} {
		var job *Job
		if source == "redis" {
			job = NewRedisSource(RedisSourceConfig{}).convertJob(&redisclient.Job{Payload: map[string]any{"command": "fixture"}, RawPayload: input})
		} else {
			job = NewAPISource(APISourceConfig{}).convertJob(&redisapi.Job{Payload: map[string]any{"command": "fixture"}, RawPayload: input})
		}
		job.RawPayload[0] = 'x'
		if input[0] != '{' {
			t.Fatal("worker metadata aliases parsed source bytes")
		}
		input[1] = 'x'
		if job.RawPayload[1] != '"' {
			t.Fatal("parsed source mutation changed worker metadata")
		}
		input[1] = '"'
	}
	jobs := []*Job{
		NewRedisSource(RedisSourceConfig{}).convertJob(&redisclient.Job{Payload: map[string]any{"command": "fixture"}}),
		NewAPISource(APISourceConfig{}).convertJob(&redisapi.Job{Payload: map[string]any{"command": "fixture"}}),
	}
	ws, err := NewWSSource(WSSourceConfig{}).convertWSJob(redisapi.WSMessage{Data: map[string]string{"jobId": "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	jobs = append(jobs, ws)
	for _, job := range jobs {
		if job.RawPayload != nil {
			t.Fatal("missing original bytes reconstructed from a map")
		}
	}
}

func TestWorkerJobMarshalExcludesRawPayload(t *testing.T) {
	const sentinel = "RAW_ONLY_PAYLOAD_SENTINEL"
	job := Job{ID: "fixture", Payload: map[string]any{"command": "fixture"}, RawPayload: []byte(sentinel)}
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
		t.Fatal("internal payload metadata leaked into serialized worker job")
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 8 || fields["ID"] != "fixture" {
		t.Fatal("public worker job representation changed")
	}
}
