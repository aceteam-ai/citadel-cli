package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	redisclient "github.com/aceteam-ai/citadel-cli/internal/redis"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/alicebob/miniredis/v2"
)

// meetingResultHandler completes a MEETING_JOIN with a fixed result.
type meetingResultHandler struct{ output map[string]any }

func (h *meetingResultHandler) CanHandle(jobType string) bool { return jobType == JobTypeMeetingJoin }

func (h *meetingResultHandler) Execute(context.Context, *Job, StreamWriter) (*JobResult, error) {
	return &JobResult{Status: JobStatusSuccess, Output: h.output}, nil
}

func meetingOutput() map[string]any {
	return map[string]any{
		"status":     "completed",
		"meeting_id": "session-1",
		"end_reason": "call_ended",
		"transcript": map[string]any{"text": "hello from the meeting", "segments": []any{map[string]any{"start": 0.0, "end": 1.5, "text": "hello"}}},
	}
}

// runMeetingToTerminal drives one MEETING_JOIN through a real Runner with the
// given factory and waits for its ack.
func runMeetingToTerminal(t *testing.T, jobID string, factory func(*Job) StreamWriter) {
	t.Helper()
	source := NewMockJobSource("retention", []*Job{{ID: jobID, RayID: "ray-1", Type: JobTypeMeetingJoin, Payload: map[string]any{}}})
	runner := NewRunner(source, []JobHandler{&meetingResultHandler{output: meetingOutput()}}, RunnerConfig{WorkerID: "w", MaxConcurrency: 1, ActivityFn: func(string, string) {}})
	runner.WithStreamWriterFactory(factory)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { runner.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(source.AckedJobs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("meeting job never acked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func assertRetainedEnd(t *testing.T, raw, jobID string) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("retained value is not JSON: %v", err)
	}
	if event["type"] != "end" || event["jobId"] != jobID || event["rayId"] != "ray-1" || event["version"] != "1.0" {
		t.Fatalf("retained envelope = %v", event)
	}
	data, _ := event["data"].(map[string]any)
	want, _ := json.Marshal(meetingOutput())
	var wantResult map[string]any
	_ = json.Unmarshal(want, &wantResult)
	if !reflect.DeepEqual(data["result"], wantResult) {
		t.Fatalf("retained result = %v, want %v", data["result"], wantResult)
	}
	return event
}

func connectMiniredis(t *testing.T) (*miniredis.Miniredis, *redisclient.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redisclient.NewClient(redisclient.ClientConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Connect(ctx, "redis://"+mr.Addr(), ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return mr, client
}

func TestMeetingJoinTerminalResultRetainedDirectRedis(t *testing.T) {
	mr, client := connectMiniredis(t)
	ctx := context.Background()
	// No subscriber on stream:v1:meeting-1: the publish reaches nobody.
	runMeetingToTerminal(t, "meeting-1", func(job *Job) StreamWriter { return redisStreamWriterFor(ctx, client, job) })

	key := TerminalResultKey("meeting-1")
	if key != "job:meeting-1:result" {
		t.Fatalf("key = %q", key)
	}
	raw, err := mr.Get(key)
	if err != nil {
		t.Fatalf("retained key missing: %v", err)
	}
	assertRetainedEnd(t, raw, "meeting-1")
	if ttl := mr.TTL(key); ttl != 72*time.Hour {
		t.Fatalf("TTL = %v, want %v", ttl, TerminalResultTTL)
	}
}

func TestSecondTerminalWriteKeepsFirstRetainedResultDirectRedis(t *testing.T) {
	mr, client := connectMiniredis(t)
	ctx := context.Background()
	w := redisStreamWriterFor(ctx, client, &Job{ID: "meeting-2", RayID: "ray-1", Type: JobTypeMeetingJoin})
	if err := w.WriteEnd(meetingOutput()); err != nil {
		t.Fatal(err)
	}
	first, _ := mr.Get(TerminalResultKey("meeting-2"))
	// A retried publish and a refused redelivery both write again.
	if err := w.WriteEnd(meetingOutput()); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteError(errMeetingRedelivered, false); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteCancelled("Meeting cancelled"); err != nil {
		t.Fatal(err)
	}
	got, _ := mr.Get(TerminalResultKey("meeting-2"))
	if got != first {
		t.Fatalf("retained result changed:\nfirst %s\ngot   %s", first, got)
	}
	assertRetainedEnd(t, got, "meeting-2")
	if keys := mr.Keys(); len(keys) != 2 { // the result plus job:<id>:status from WriteCancelled
		t.Fatalf("keys = %v", keys)
	}
}

func TestNonMeetingJobTerminalNotRetained(t *testing.T) {
	mr, client := connectMiniredis(t)
	w := redisStreamWriterFor(context.Background(), client, &Job{ID: "shell-1", Type: JobTypeShellCommand})
	if err := w.WriteEnd(map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if mr.Exists(TerminalResultKey("shell-1")) {
		t.Fatal("non-meeting terminal result was retained")
	}
}

// fakeKVProxy is a minimal Redis API proxy: pub/sub publish plus the KV route
// with its real ttl and size limits.
type fakeKVProxy struct {
	mu        sync.Mutex
	kv        map[string]string
	ttl       map[string]int
	posts     int
	published []string
	failKV    bool
}

func newFakeKVProxy(t *testing.T) (*fakeKVProxy, *redisapi.Client) {
	t.Helper()
	p := &fakeKVProxy{kv: map[string]string{}, ttl: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(srv.Close)
	client := redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "synthetic-token"})
	t.Cleanup(func() { client.Close() })
	return p, client
}

func (p *fakeKVProxy) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/pubsub/publish"):
		var body struct{ Message string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.published = append(p.published, body.Message)
		_, _ = w.Write([]byte(`{"success":true,"published":false}`))
	case strings.HasSuffix(r.URL.Path, "/redis/kv") && r.Method == http.MethodGet:
		key := r.URL.Query().Get("key")
		v, ok := p.kv[key]
		if !ok {
			_, _ = w.Write([]byte(fmt.Sprintf(`{"key":%q,"value":null,"ttl":-2}`, key)))
			return
		}
		out, _ := json.Marshal(map[string]any{"key": key, "value": v, "ttl": p.ttl[key]})
		_, _ = w.Write(out)
	case strings.HasSuffix(r.URL.Path, "/redis/kv") && r.Method == http.MethodPost:
		p.posts++
		var body struct {
			Key   string `json:"key"`
			Value string `json:"value"`
			TTL   int    `json:"ttl"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if p.failKV {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if body.TTL > 86400 || len(body.Value) > 1024*1024 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.kv[body.Key], p.ttl[body.Key] = body.Value, body.TTL
		_, _ = w.Write([]byte(`{"success":true}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func TestMeetingJoinTerminalResultRetainedAPIMode(t *testing.T) {
	proxy, client := newFakeKVProxy(t)
	ctx := context.Background()
	runMeetingToTerminal(t, "meeting-3", func(job *Job) StreamWriter { return apiStreamWriterFor(ctx, client, job) })

	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	key := TerminalResultKey("meeting-3")
	raw, ok := proxy.kv[key]
	if !ok {
		t.Fatalf("retained key missing; kv = %v", proxy.kv)
	}
	assertRetainedEnd(t, raw, "meeting-3")
	if proxy.ttl[key] != 86400 {
		t.Fatalf("ttl = %d, want the proxy maximum 86400", proxy.ttl[key])
	}
	// The retained value is the envelope the HTTP publish sent.
	var published string
	for _, m := range proxy.published {
		if strings.Contains(m, `"type":"end"`) {
			published = m
		}
	}
	if published != raw {
		t.Fatalf("retained value differs from published end event:\npublished %s\nretained  %s", published, raw)
	}
}

func TestSecondTerminalWriteKeepsFirstRetainedResultAPIMode(t *testing.T) {
	proxy, client := newFakeKVProxy(t)
	w := apiStreamWriterFor(context.Background(), client, &Job{ID: "meeting-4", RayID: "ray-1", Type: JobTypeMeetingJoin})
	if err := w.WriteEnd(meetingOutput()); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteError(errMeetingRedelivered, false); err != nil {
		t.Fatal(err)
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.posts != 1 {
		t.Fatalf("kv posts = %d, want 1", proxy.posts)
	}
	assertRetainedEnd(t, proxy.kv[TerminalResultKey("meeting-4")], "meeting-4")
}

func TestRetentionFailureDoesNotFailTerminalPublishAPIMode(t *testing.T) {
	proxy, client := newFakeKVProxy(t)
	proxy.failKV = true
	var logged []string
	orig := retentionLogf
	retentionLogf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { retentionLogf = orig })

	w := apiStreamWriterFor(context.Background(), client, &Job{ID: "meeting-5", Type: JobTypeMeetingJoin})
	if err := w.WriteEnd(meetingOutput()); err != nil {
		t.Fatalf("terminal publish failed because retention failed: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "meeting-5") {
		t.Fatalf("logged = %v", logged)
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if len(proxy.published) != 1 {
		t.Fatalf("published = %d, want 1", len(proxy.published))
	}
}

func TestDurableAPIWriterDoesNotRetain(t *testing.T) {
	proxy, client := newFakeKVProxy(t)
	w := apiStreamWriterFor(context.Background(), client, &Job{ID: "meeting-6", Type: JobTypeMeetingJoin})
	w.durable = true
	_ = w.WriteEnd(meetingOutput())
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.posts != 0 {
		t.Fatalf("durable writer retained (%d posts)", proxy.posts)
	}
}

func TestAPIRetainRefusesOversizeValueLocally(t *testing.T) {
	proxy, client := newFakeKVProxy(t)
	big := []byte(`"` + strings.Repeat("x", redisapi.KVMaxValueBytes) + `"`)
	stored, err := client.RetainTerminalEvent(context.Background(), TerminalResultKey("meeting-7"), big, TerminalResultTTL)
	if stored || !errors.Is(err, redisapi.ErrValueTooLarge) {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if proxy.posts != 0 {
		t.Fatal("oversize value was sent to the proxy")
	}
}
