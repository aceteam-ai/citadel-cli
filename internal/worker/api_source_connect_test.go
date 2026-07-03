package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// TestAPISource_Connect_RetriesAfterFailedPing verifies that a failed ping does
// NOT leave a client cached (which would make the next Connect short-circuit and
// falsely report success). The connect-with-backoff loop (#443) relies on
// Connect actually re-pinging on each call.
func TestAPISource_Connect_RetriesAfterFailedPing(t *testing.T) {
	var pings int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&pings, 1)
		if n <= 2 {
			// First two pings fail (simulate transient outage / rate limit).
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Rate limit exceeded","limit":50000,"window":"day","retry_after":1}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewAPISource(APISourceConfig{BaseURL: srv.URL, Token: "t"})
	ctx := context.Background()

	// First attempt: 429. Must return an error that unwraps to RateLimitError,
	// and must NOT retain the client.
	err := s.Connect(ctx)
	if err == nil {
		t.Fatal("first Connect returned nil, want error on 429")
	}
	if _, ok := redisapi.AsRateLimitError(err); !ok {
		t.Errorf("first Connect error does not unwrap to RateLimitError: %v", err)
	}
	if s.client != nil {
		t.Fatal("client retained after failed ping; a retry would short-circuit and skip re-ping")
	}

	// Second attempt: still 429.
	if err := s.Connect(ctx); err == nil {
		t.Fatal("second Connect returned nil, want error on 429")
	}
	if s.client != nil {
		t.Fatal("client retained after second failed ping")
	}

	// Third attempt: success. Client is retained.
	if err := s.Connect(ctx); err != nil {
		t.Fatalf("third Connect returned error, want success: %v", err)
	}
	if s.client == nil {
		t.Fatal("client not retained after successful ping")
	}
	if got := atomic.LoadInt32(&pings); got != 3 {
		t.Errorf("ping hit %d times, want 3 (each Connect re-pings until success)", got)
	}

	// Fourth attempt: already connected, should short-circuit without a ping.
	if err := s.Connect(ctx); err != nil {
		t.Fatalf("fourth Connect (already connected) returned error: %v", err)
	}
	if got := atomic.LoadInt32(&pings); got != 3 {
		t.Errorf("ping hit %d times after already-connected Connect, want 3 (no extra ping)", got)
	}
}
