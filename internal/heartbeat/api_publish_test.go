package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

const (
	pubSubPath = "/api/fabric/redis/pubsub/publish"
	streamPath = "/api/fabric/redis/streams/add"
)

// redisAPIStub stands in for the AceTeam Redis API proxy, letting each of the
// two heartbeat destinations be failed independently, which is the whole
// point of citadel-cli#722.
type redisAPIStub struct {
	mu sync.Mutex

	failPubSub bool
	failStream bool

	pubSubCalls int
	streamCalls int
	// streamPayloads holds the decoded "payload" field of each stream write,
	// so a test can prove the heartbeat actually landed rather than merely
	// that some request arrived.
	streamPayloads []StatusMessage
}

func (s *redisAPIStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc(pubSubPath, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.pubSubCalls++
		fail := s.failPubSub
		s.mu.Unlock()

		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"pubsub down"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "receivers": 1})
	})

	mux.HandleFunc(streamPath, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream string            `json:"stream"`
			Values map[string]string `json:"values"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		s.mu.Lock()
		s.streamCalls++
		fail := s.failStream
		if !fail {
			var msg StatusMessage
			if err := json.Unmarshal([]byte(body.Values["payload"]), &msg); err == nil {
				s.streamPayloads = append(s.streamPayloads, msg)
			}
		}
		s.mu.Unlock()

		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"stream down"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "messageId": "1-0"})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *redisAPIStub) counts() (pubSub, stream int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pubSubCalls, s.streamCalls
}

func (s *redisAPIStub) landed() []StatusMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]StatusMessage(nil), s.streamPayloads...)
}

// newTestAPIPublisher wires an APIPublisher at the stub server and captures its
// operator-facing log lines.
func newTestAPIPublisher(t *testing.T, stub *redisAPIStub) (*APIPublisher, *[]string) {
	t.Helper()
	srv := stub.server(t)

	var mu sync.Mutex
	var logs []string

	pub, err := NewAPIPublisher(APIPublisherConfig{
		Client:          redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "test-token"}),
		NodeID:          "test-node",
		HeadscaleNodeID: "758",
		OrgID:           "org-1",
		LogFn: func(level, msg string) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, level+": "+msg)
		},
	}, status.NewCollector(status.CollectorConfig{NodeName: "test-node"}))
	if err != nil {
		t.Fatalf("NewAPIPublisher: %v", err)
	}
	return pub, &logs
}

func testStatusMessage() StatusMessage {
	return StatusMessage{
		Version:         "1.0",
		Timestamp:       "2026-08-10T00:00:00Z",
		NodeID:          "test-node",
		HeadscaleNodeID: "758",
		Status:          &status.NodeStatus{Version: "1.0"},
	}
}

// TestPublishMessagePubSubFailureIsNotFatal is the regression test for #722: a
// failing best-effort pub/sub publish must NOT prevent the durable stream write
// and must NOT be reported as a heartbeat failure. Before the fix, publishStatus
// returned early on the pub/sub error and the XADD never ran, so a healthy node
// went stale on the platform and fail-closed routing dropped it from the fabric.
func TestPublishMessagePubSubFailureIsNotFatal(t *testing.T) {
	stub := &redisAPIStub{failPubSub: true}
	pub, logs := newTestAPIPublisher(t, stub)

	msg := testStatusMessage()
	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
		t.Fatalf("pub/sub failure must not fail the heartbeat, got: %v", err)
	}

	pubSubCalls, streamCalls := stub.counts()
	if pubSubCalls != 1 {
		t.Errorf("pub/sub publish should still be attempted, got %d calls", pubSubCalls)
	}
	if streamCalls != 1 {
		t.Fatalf("durable stream write must happen despite pub/sub failure, got %d calls", streamCalls)
	}

	landed := stub.landed()
	if len(landed) != 1 {
		t.Fatalf("expected 1 heartbeat on the durable stream, got %d", len(landed))
	}
	if landed[0].NodeID != "test-node" || landed[0].Timestamp != msg.Timestamp {
		t.Errorf("durable stream payload mismatch: %+v", landed[0])
	}

	// A sustained failure must be visible somewhere: the first failure of a run
	// is always reported.
	if !containsSubstring(*logs, "pub/sub publish to node:status:org:org-1:test-node failing (1st consecutive failure)") {
		t.Errorf("first pub/sub failure must be surfaced without a misleading 0s duration, logs: %v", *logs)
	}
}

// TestPublishMessageStreamFailureIsFatal proves the severity is now on the
// right write: losing the durable stream is the error the caller must see.
// Before the fix this was logged and swallowed with a nil return.
func TestPublishMessageStreamFailureIsFatal(t *testing.T) {
	stub := &redisAPIStub{failStream: true}
	pub, _ := newTestAPIPublisher(t, stub)

	msg := testStatusMessage()
	err := pub.publishMessage(context.Background(), msg, msg.Timestamp)
	if err == nil {
		t.Fatal("a failed durable stream write must return an error")
	}
	if !strings.Contains(err.Error(), "node:status:stream") {
		t.Errorf("error should name the durable stream, got: %v", err)
	}

	// The best-effort publish is still attempted; the two paths fail
	// independently in API mode, so a stream outage must not also blind the UI.
	pubSubCalls, streamCalls := stub.counts()
	if pubSubCalls != 1 {
		t.Errorf("pub/sub publish should still be attempted when the stream fails, got %d calls", pubSubCalls)
	}
	if streamCalls != 1 {
		t.Errorf("expected 1 stream attempt, got %d", streamCalls)
	}
}

// TestPublishMessageBothFailReportsStreamError pins which error wins when
// everything is down: the durable one, since that is the actionable failure.
func TestPublishMessageBothFailReportsStreamError(t *testing.T) {
	stub := &redisAPIStub{failPubSub: true, failStream: true}
	pub, _ := newTestAPIPublisher(t, stub)

	msg := testStatusMessage()
	err := pub.publishMessage(context.Background(), msg, msg.Timestamp)
	if err == nil {
		t.Fatal("expected an error when the durable stream write fails")
	}
	if !strings.Contains(err.Error(), "durable stream") {
		t.Errorf("the durable stream failure must be the reported one, got: %v", err)
	}
}

// TestPublishMessageHappyPath keeps the ordinary case honest: both writes
// happen, no error, and nothing is logged at operator level.
func TestPublishMessageHappyPath(t *testing.T) {
	stub := &redisAPIStub{}
	pub, logs := newTestAPIPublisher(t, stub)

	msg := testStatusMessage()
	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
		t.Fatalf("publishMessage: %v", err)
	}

	pubSubCalls, streamCalls := stub.counts()
	if pubSubCalls != 1 || streamCalls != 1 {
		t.Errorf("expected 1 pub/sub and 1 stream call, got %d and %d", pubSubCalls, streamCalls)
	}
	if len(*logs) != 0 {
		t.Errorf("a healthy heartbeat must not log, got: %v", *logs)
	}
}

// TestAPIPublisherOnStatusFansOut is the API-mode half of citadel #612's
// wiring: SetOnStatus must accumulate callbacks, not have the second
// registration clobber the first. Both the #416 auto-stop reconciler and the
// #612 inference-queue reconciler register on the same publisher, so a
// regression here silently drops one of them.
func TestAPIPublisherOnStatusFansOut(t *testing.T) {
	stub := &redisAPIStub{}
	pub, _ := newTestAPIPublisher(t, stub)

	var mu sync.Mutex
	var calls []string
	pub.SetOnStatus(func(_ *status.NodeStatus) {
		mu.Lock()
		calls = append(calls, "auto-stop")
		mu.Unlock()
	})
	pub.SetOnStatus(func(_ *status.NodeStatus) {
		mu.Lock()
		calls = append(calls, "inference-queue")
		mu.Unlock()
	})

	if got := len(pub.onStatusFns); got != 2 {
		t.Fatalf("SetOnStatus must accumulate registrations, got %d callbacks registered (want 2)", got)
	}

	if err := pub.publishStatus(context.Background()); err != nil {
		t.Fatalf("publishStatus: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("want both OnStatus callbacks invoked per publish, got %v", calls)
	}
}

// TestPublishMessageRecordsMarkerOnSuccess pins the wiring half of #726
// (API-mode side): a successful durable stream write must update the on-disk
// freshness marker.
func TestPublishMessageRecordsMarkerOnSuccess(t *testing.T) {
	stub := &redisAPIStub{}
	pub, _ := newTestAPIPublisher(t, stub)
	dir := t.TempDir()
	pub.markerDir = dir

	before := time.Now()
	msg := testStatusMessage()
	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
		t.Fatalf("publishMessage: %v", err)
	}

	m := LoadMarker(dir)
	if m.LastSuccessAt.Before(before) {
		t.Fatalf("LastSuccessAt = %v, want >= %v", m.LastSuccessAt, before)
	}
}

// TestPublishMessageDoesNotMoveMarkerOnFailure mirrors the direct-Redis test
// of the same name: a failed durable write must not advance LastSuccessAt.
func TestPublishMessageDoesNotMoveMarkerOnFailure(t *testing.T) {
	stub := &redisAPIStub{failStream: true}
	pub, _ := newTestAPIPublisher(t, stub)
	dir := t.TempDir()
	pub.markerDir = dir

	msg := testStatusMessage()
	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err == nil {
		t.Fatal("expected the durable stream write to fail")
	}

	m := LoadMarker(dir)
	if !m.LastSuccessAt.IsZero() {
		t.Fatalf("expected LastSuccessAt to stay zero after an all-failure run, got %v", m.LastSuccessAt)
	}
	if m.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", m.ConsecutiveFailures)
	}
}

// TestPublishMessageWarningIsRateLimitedThenRecovers exercises the noise
// policy end to end through the publisher: a sustained pub/sub outage logs once
// (not once per 30s interval), and the recovery is announced.
func TestPublishMessageWarningIsRateLimitedThenRecovers(t *testing.T) {
	stub := &redisAPIStub{failPubSub: true}
	pub, logs := newTestAPIPublisher(t, stub)

	msg := testStatusMessage()
	for range 20 {
		if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
			t.Fatalf("pub/sub failure must not fail the heartbeat: %v", err)
		}
	}
	if got := len(*logs); got != 1 {
		t.Fatalf("20 consecutive pub/sub failures should log once within the rate-limit window, got %d: %v", got, *logs)
	}

	stub.mu.Lock()
	stub.failPubSub = false
	stub.mu.Unlock()

	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
		t.Fatalf("publishMessage: %v", err)
	}
	if got := len(*logs); got != 2 {
		t.Fatalf("recovery should be logged exactly once, got %d lines: %v", got, *logs)
	}
	if !strings.Contains((*logs)[1], "recovered after 20 consecutive failures") {
		t.Errorf("recovery line should carry the failure count, got: %s", (*logs)[1])
	}

	// And it stays quiet once healthy.
	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
		t.Fatalf("publishMessage: %v", err)
	}
	if got := len(*logs); got != 2 {
		t.Errorf("a healthy heartbeat must not re-log recovery, got %d lines: %v", got, *logs)
	}
}

// TestPubSubHealthRateLimit pins the rate-limit policy directly, since the
// 15-minute window is impractical to reach through the publisher.
func TestPubSubHealthRateLimit(t *testing.T) {
	var h pubSubHealth
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	if report, escalate := h.recordFailure(start); !escalate || report.Failures != 1 {
		t.Fatalf("first failure must be reported, got escalate=%v report=%+v", escalate, report)
	}
	// Every 30s for just under the window: suppressed.
	now := start
	for now.Sub(start) < pubSubWarnInterval-30*time.Second {
		now = now.Add(30 * time.Second)
		if _, escalate := h.recordFailure(now); escalate {
			t.Fatalf("failure at %s should be suppressed inside the window", now.Sub(start))
		}
	}
	// Crossing the window escalates again, carrying the elapsed duration.
	now = now.Add(30 * time.Second)
	report, escalate := h.recordFailure(now)
	if !escalate {
		t.Fatalf("failure at %s should escalate after the window", now.Sub(start))
	}
	if report.Duration != pubSubWarnInterval {
		t.Errorf("report should carry the outage duration, got %s", report.Duration)
	}
	if report.Failures != 31 {
		t.Errorf("report should carry the consecutive failure count, got %d", report.Failures)
	}
	if got := h.consecutiveFailures(); got != 31 {
		t.Errorf("consecutiveFailures() = %d, want 31", got)
	}

	// Recovery reports once, then resets.
	rec, recovered := h.recordSuccess(now.Add(30 * time.Second))
	if !recovered || rec.Failures != 31 {
		t.Fatalf("recovery should be reported once with the run length, got %v %+v", recovered, rec)
	}
	if _, recovered := h.recordSuccess(now.Add(time.Minute)); recovered {
		t.Error("a second success must not re-report recovery")
	}
	if got := h.consecutiveFailures(); got != 0 {
		t.Errorf("consecutiveFailures() after recovery = %d, want 0", got)
	}

	// A new run escalates immediately rather than inheriting the old window.
	if _, escalate := h.recordFailure(now.Add(2 * time.Minute)); !escalate {
		t.Error("the first failure of a NEW run must be reported immediately")
	}
}

func containsSubstring(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
