package redisapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLastConsumeStatusCaptures verifies that the HTTP status of a consume call
// is recorded and exposed via LastConsumeStatus(). This is the signal that
// would have surfaced the pre-fix #3924 400s (issue #236).
func TestLastConsumeStatusCaptures(t *testing.T) {
	var statusToReturn int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusToReturn)
		if statusToReturn >= 400 {
			w.Write([]byte(`{"error":"bad consumer group"}`))
		} else {
			w.Write([]byte(`{"messages":[]}`))
		}
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	if c.LastConsumeStatus() != 0 {
		t.Fatalf("expected 0 before any consume, got %d", c.LastConsumeStatus())
	}

	// A 400 must be recorded even though ConsumeJob returns an error.
	statusToReturn = http.StatusBadRequest
	_, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "q", Group: "g", Consumer: "w", Count: 1, BlockMs: 100})
	if err == nil {
		t.Fatalf("expected error on 400 consume")
	}
	if c.LastConsumeStatus() != http.StatusBadRequest {
		t.Fatalf("expected last consume status 400, got %d", c.LastConsumeStatus())
	}

	// A subsequent 200 must update the recorded status.
	statusToReturn = http.StatusOK
	if _, err := c.ConsumeJob(context.Background(), ConsumeRequest{Queue: "q", Group: "g", Consumer: "w", Count: 1, BlockMs: 100}); err != nil {
		t.Fatalf("unexpected error on 200 consume: %v", err)
	}
	if c.LastConsumeStatus() != http.StatusOK {
		t.Fatalf("expected last consume status 200, got %d", c.LastConsumeStatus())
	}
}
