package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// A single 429 with Retry-After: 1 on the durable stream write must be retried
// once inside the same tick, so the heartbeat lands without waiting an interval.
func TestPublishMessageRecoversFromSingle429WithinTick(t *testing.T) {
	var streamCalls, pubSubCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, _ *http.Request) {
		if streamCalls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Rate limit exceeded"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "messageId": "1-0"})
	})
	mux.HandleFunc(pubSubPath, func(w http.ResponseWriter, _ *http.Request) {
		pubSubCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pub, err := NewAPIPublisher(APIPublisherConfig{
		Client:   redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "t"}),
		NodeID:   "n",
		OrgID:    "o",
		Interval: 30 * time.Second,
	}, status.NewCollector(status.CollectorConfig{NodeName: "n"}))
	if err != nil {
		t.Fatal(err)
	}

	msg := testStatusMessage()
	start := time.Now()
	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err != nil {
		t.Fatalf("heartbeat should recover after one 429: %v", err)
	}
	if el := time.Since(start); el < time.Second || el > 10*time.Second {
		t.Fatalf("retry should wait ~Retry-After, took %s", el)
	}
	if got := streamCalls.Load(); got != 2 {
		t.Fatalf("want exactly one retry (2 stream calls), got %d", got)
	}
	if pubSubCalls.Load() != 1 {
		t.Fatalf("pub/sub should run once after recovery, got %d", pubSubCalls.Load())
	}
}

// A Retry-After longer than half the interval is not waited out in-tick, and a
// persistent 429 gets one retry, never a storm; pub/sub is skipped while limited.
func TestPublishMessage429BoundedNoStorm(t *testing.T) {
	var streamCalls, pubSubCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc(streamPath, func(w http.ResponseWriter, _ *http.Request) {
		streamCalls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	mux.HandleFunc(pubSubPath, func(w http.ResponseWriter, _ *http.Request) { pubSubCalls.Add(1) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	pub, err := NewAPIPublisher(APIPublisherConfig{
		Client:   redisapi.NewClient(redisapi.ClientConfig{BaseURL: srv.URL, Token: "t"}),
		NodeID:   "n",
		OrgID:    "o",
		Interval: 30 * time.Second,
	}, status.NewCollector(status.CollectorConfig{NodeName: "n"}))
	if err != nil {
		t.Fatal(err)
	}
	msg := testStatusMessage()
	if err := pub.publishMessage(context.Background(), msg, msg.Timestamp); err == nil {
		t.Fatal("a persistent 429 must still report failure")
	}
	if got := streamCalls.Load(); got != 2 {
		t.Fatalf("want 1 attempt + 1 retry, got %d", got)
	}
	if pubSubCalls.Load() != 0 {
		t.Fatal("pub/sub must be skipped while rate limited")
	}

	// Hint beyond interval/2: no in-tick wait at all.
	streamCalls.Store(0)
	pub.interval = time.Second // half = 500ms < 1s hint
	start := time.Now()
	_ = pub.publishMessage(context.Background(), msg, msg.Timestamp)
	if streamCalls.Load() != 1 || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("oversized hint must not be waited out (calls=%d, %s)", streamCalls.Load(), time.Since(start))
	}
}
