package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/usage"
	goredis "github.com/redis/go-redis/v9"
)

const fileFetchPrivacyToken = "synthetic-delivery.synthetic-credential"

type fileFetchPrivacyFailureHandler struct{ status JobStatus }

func (h fileFetchPrivacyFailureHandler) CanHandle(kind string) bool { return kind == JobTypeFileFetch }
func (h fileFetchPrivacyFailureHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	err := errors.New("delivery failed: " + fileFetchPrivacyToken)
	return &JobResult{Status: h.status, Error: err}, nil
}

func TestFileFetchRunnerRetainedFailuresRedactCredentials(t *testing.T) {
	for _, status := range []JobStatus{JobStatusFailure, JobStatusRetry, JobStatusTerminalFailure} {
		t.Run(string(status), func(t *testing.T) {
			var retained []string
			source := NewMockJobSource("privacy", nil)
			runner := NewRunner(source, []JobHandler{fileFetchPrivacyFailureHandler{status}}, RunnerConfig{ActivityFn: func(_ string, message string) { retained = append(retained, message) }})
			runner.jobRecordFn = func(record usage.UsageRecord) { retained = append(retained, record.ErrorMessage) }
			stream := &MockStreamWriter{}
			job := &Job{ID: "privacy-job", Type: JobTypeFileFetch, Payload: map[string]any{"capability": fileFetchPrivacyToken}}
			runner.executeJob(context.Background(), job, stream, time.Now(), false, 0)
			if stream.erroredErr != nil {
				retained = append(retained, stream.erroredErr.Error())
			}
			if strings.Contains(strings.Join(retained, "\n"), fileFetchPrivacyToken) {
				t.Fatal("runner retained a delivery credential in failure/retry diagnostics")
			}
		})
	}
}

func TestFileFetchJobDiagnosticsOmitCredentials(t *testing.T) {
	payload := map[string]any{"capability": fileFetchPrivacyToken, "dest_path": "chapter.mp4"}
	raw, _ := json.Marshal(payload)
	for _, job := range []any{
		&Job{Type: JobTypeFileFetch, Payload: payload, RawPayload: raw},
		&redisclient.Job{Type: JobTypeFileFetch, Payload: payload, RawPayload: raw, RawData: map[string]any{"payload": string(raw)}},
		&redisapi.Job{Type: JobTypeFileFetch, Payload: payload, RawPayload: raw, RawData: map[string]any{"payload": string(raw)}},
	} {
		encoded, err := json.Marshal(job)
		if err != nil || strings.Contains(string(encoded), fileFetchPrivacyToken) {
			t.Fatal("retained job envelope contains a delivery credential")
		}
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
			if strings.Contains(fmt.Sprintf(format, job), fileFetchPrivacyToken) {
				t.Fatal("job diagnostic contains a delivery credential")
			}
		}
	}
	if payload["capability"] != fileFetchPrivacyToken {
		t.Fatal("diagnostics mutated execution authority")
	}
}

// This starts an isolated real Redis process with no TCP listener or persistence.
// It never contacts a node or an operator's Redis instance.
func fileFetchDisposableRedis(t *testing.T) (*goredis.Client, string) {
	t.Helper()
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable for disposable Redis retention integration")
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "r.sock")
	server := exec.Command(binary, "--port", "0", "--unixsocket", socket, "--unixsocketperm", "700", "--save", "", "--appendonly", "no", "--dir", dir)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	raw := goredis.NewClient(&goredis.Options{Network: "unix", Addr: socket})
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for raw.Ping(ctx).Err() != nil {
		if ctx.Err() != nil {
			t.Fatal("disposable Redis did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return raw, "unix://" + socket
}

func TestFileFetchDirectRedisRetentionLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "failure", "retry", "unsupported", "max_attempts_single", "max_attempts_multi"} {
		t.Run(mode, func(t *testing.T) {
			raw, url := fileFetchDisposableRedis(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			queue := "jobs:v1:privacy:node:fixture"
			var logs []string
			cfg := RedisSourceConfig{URL: url, QueueName: queue, ConsumerGroup: "privacy-workers", BlockMs: 1, MaxAttempts: 3,
				LogFn: func(_ string, message string) { logs = append(logs, message) }}
			if strings.HasPrefix(mode, "max_attempts") {
				cfg.MaxAttempts = 1
			}
			if mode == "max_attempts_multi" {
				cfg.QueueNames = []string{"jobs:v1:privacy:unused", queue}
			}
			source := NewRedisSource(cfg)
			if err := source.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			payload, _ := json.Marshal(map[string]any{"capability": fileFetchPrivacyToken, "dest_path": "chapter.mp4"})
			messageID := enqueueTestJob(t, raw, queue, map[string]any{"jobId": "privacy-job", "type": JobTypeFileFetch, "payload": string(payload)})
			job, err := source.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "max_attempts") {
				if job != nil {
					t.Fatal("max-attempts delivery reached a handler")
				}
				dlq := "dlq:v1:fixture"
				if mode == "max_attempts_multi" {
					dlq = "dlq:v1:privacy:node:fixture"
				}
				entries, err := raw.XRange(ctx, dlq, "-", "+").Result()
				if err != nil || len(entries) != 1 || entries[0].Values["payload"] != "null" {
					t.Fatal("pre-handler failure did not retain exactly one sanitized dead letter")
				}
			} else {
				if job == nil || job.Payload["capability"] != fileFetchPrivacyToken {
					t.Fatal("in-flight delivery lost its execution credential")
				}
				switch mode {
				case "failure":
					err = source.Fail(ctx, job, errors.New("fetch failed: "+fileFetchPrivacyToken), map[string]any{"capability": fileFetchPrivacyToken, "payload": job.Payload})
				case "unsupported":
					runner := NewRunner(source, nil, RunnerConfig{ActivityFn: func(_ string, message string) { logs = append(logs, message) }})
					runner.executeJob(ctx, job, &NoOpStreamWriter{}, time.Now(), false, 0)
				case "retry":
					err = source.Nack(ctx, job, errors.New("retry: "+fileFetchPrivacyToken))
					entries, readErr := raw.XRange(ctx, queue, "-", "+").Result()
					if readErr != nil || len(entries) != 1 || entries[0].ID != messageID {
						t.Fatal("retry duplicated or removed the original in-flight delivery")
					}
					if err == nil {
						err = source.Ack(ctx, job)
					}
				default:
					err = source.Ack(ctx, job)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			entries, err := raw.XRange(ctx, queue, messageID, messageID).Result()
			if err != nil || len(entries) != 0 {
				t.Fatal("acknowledged FILE_FETCH source entry was retained")
			}
			pending, err := raw.XPending(ctx, queue, cfg.ConsumerGroup).Result()
			if err != nil || pending.Count != 0 {
				t.Fatal("acknowledged FILE_FETCH remained pending")
			}
			keys, err := raw.Keys(ctx, "*").Result()
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				var retained any
				switch raw.Type(ctx, key).Val() {
				case "stream":
					retained, err = raw.XRange(ctx, key, "-", "+").Result()
				case "hash":
					retained, err = raw.HGetAll(ctx, key).Result()
				}
				if err != nil || strings.Contains(fmt.Sprint(retained), fileFetchPrivacyToken) {
					t.Fatal("retained Redis stream/status contains delivery credential")
				}
			}
			if strings.Contains(strings.Join(logs, "\n"), fileFetchPrivacyToken) {
				t.Fatal("worker diagnostics contain delivery credential")
			}
		})
	}
}

func TestFileFetchDirectRedisAckScope(t *testing.T) {
	raw, url := fileFetchDisposableRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queue := "jobs:v1:privacy:node:fixture"
	source := NewRedisSource(RedisSourceConfig{URL: url, QueueName: queue, ConsumerGroup: "workers", BlockMs: 1, LogFn: func(string, string) {}})
	if err := source.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	id := enqueueTestJob(t, raw, queue, map[string]any{"jobId": "fixture", "type": JobTypeFileFetch, "payload": `{}`})
	if err := source.Client().AckFileFetchJob(ctx, queue, id); err != nil {
		t.Fatal(err)
	}
	if rows, err := raw.XRange(ctx, queue, id, id).Result(); err != nil || len(rows) != 1 {
		t.Fatal("ACK without a pending delivery deleted the source entry")
	}
	job, err := source.Next(ctx)
	if err != nil || job == nil {
		t.Fatal("pending-delivery fixture failed")
	}
	if err := source.Ack(ctx, job); err != nil {
		t.Fatal(err)
	}
	if rows, err := raw.XRange(ctx, queue, id, id).Result(); err != nil || len(rows) != 0 {
		t.Fatal("successful FILE_FETCH ACK retained the source entry")
	}
	id = enqueueTestJob(t, raw, queue, map[string]any{"jobId": "ordinary", "type": JobTypeFileWriteBytes, "payload": `{}`})
	job, err = source.Next(ctx)
	if err != nil || job == nil {
		t.Fatal("ordinary-job fixture failed")
	}
	// Even a caller selecting the disposable ACK method cannot delete a
	// different job type; classification comes from the original Redis entry.
	if err := source.Client().AckFileFetchJob(ctx, queue, id); err != nil {
		t.Fatal(err)
	}
	if rows, err := raw.XRange(ctx, queue, id, id).Result(); err != nil || len(rows) != 1 {
		t.Fatal("ordinary job retention changed")
	}
}

func TestFileFetchAPIAndWSFailureStatusRedaction(t *testing.T) {
	for _, transport := range []string{"api", "ws"} {
		t.Run(transport, func(t *testing.T) {
			var writes []string
			var mu sync.Mutex
			record := func(message string) { mu.Lock(); defer mu.Unlock(); writes = append(writes, message) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var data any
				_ = json.NewDecoder(request.Body).Decode(&data)
				record(fmt.Sprint(data))
				fmt.Fprint(w, `{"acknowledged":true}`)
			}))
			defer srv.Close()
			client := redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "synthetic-device", DebugFunc: func(format string, args ...any) { record(fmt.Sprintf(format, args...)) }})
			var source JobSource
			if transport == "api" {
				s := NewAPISource(APISourceConfig{QueueName: "jobs:v1:fixture"})
				s.client = client
				source = s
			} else {
				s := NewWSSource(WSSourceConfig{QueueName: "jobs:v1:fixture"})
				s.client = client
				source = s
			}
			job := &Job{ID: "privacy-job", Type: JobTypeFileFetch, MessageID: "1-0", Payload: map[string]any{"capability": fileFetchPrivacyToken}}
			err := errors.New("failure: " + fileFetchPrivacyToken)
			if err := source.Nack(context.Background(), job, err); err != nil {
				t.Fatal(err)
			}
			if err := source.Fail(context.Background(), job, err, map[string]any{"payload": job.Payload}); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(strings.Join(writes, "\n"), fileFetchPrivacyToken) {
				t.Fatal("failure/retry status or diagnostics retained delivery credential")
			}
		})
	}
}
