package redisapi

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestParseRateLimitError_FullBody(t *testing.T) {
	body := `{"error":"Rate limit exceeded","limit":50000,"window":"day",` +
		`"retry_after":86400,"reset_at":"2026-07-04T02:28:33Z"}`

	e := parseRateLimitError(429, body)
	if e.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", e.StatusCode)
	}
	if e.Message != "Rate limit exceeded" {
		t.Errorf("Message = %q, want %q", e.Message, "Rate limit exceeded")
	}
	if e.Limit != 50000 {
		t.Errorf("Limit = %d, want 50000", e.Limit)
	}
	if e.Window != "day" {
		t.Errorf("Window = %q, want day", e.Window)
	}
	if e.RetryAfter != 86400*time.Second {
		t.Errorf("RetryAfter = %s, want 86400s", e.RetryAfter)
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-07-04T02:28:33Z")
	if !e.ResetAt.Equal(wantReset) {
		t.Errorf("ResetAt = %s, want %s", e.ResetAt, wantReset)
	}
}

func TestParseRateLimitError_EmptyBody(t *testing.T) {
	// Must still return a usable typed error, never nil, so the connect loop
	// can treat any 429 as rate-limited.
	e := parseRateLimitError(429, "")
	if e == nil {
		t.Fatal("parseRateLimitError returned nil for empty body")
	}
	if e.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", e.StatusCode)
	}
	if e.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s, want 0", e.RetryAfter)
	}
}

func TestRateLimitError_Wait(t *testing.T) {
	now := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)

	// retry_after takes precedence.
	e := &RateLimitError{RetryAfter: 30 * time.Second, ResetAt: now.Add(time.Hour)}
	if got := e.Wait(now); got != 30*time.Second {
		t.Errorf("Wait (retry_after) = %s, want 30s", got)
	}

	// Falls back to reset_at when no retry_after.
	e = &RateLimitError{ResetAt: now.Add(10 * time.Minute)}
	if got := e.Wait(now); got != 10*time.Minute {
		t.Errorf("Wait (reset_at) = %s, want 10m", got)
	}

	// Past reset_at yields 0 (no hint) so caller falls back to its own backoff.
	e = &RateLimitError{ResetAt: now.Add(-time.Minute)}
	if got := e.Wait(now); got != 0 {
		t.Errorf("Wait (past reset) = %s, want 0", got)
	}

	// No hints at all -> 0.
	e = &RateLimitError{}
	if got := e.Wait(now); got != 0 {
		t.Errorf("Wait (no hints) = %s, want 0", got)
	}
}

func TestAsRateLimitError(t *testing.T) {
	rle := parseRateLimitError(429, `{"error":"nope"}`)

	// Wrapped in a chain (mirrors the %w wrap in APISource.Connect / Ping).
	wrapped := fmt.Errorf("failed to connect to Redis API: %w", rle)
	got, ok := AsRateLimitError(wrapped)
	if !ok {
		t.Fatal("AsRateLimitError did not unwrap a wrapped RateLimitError")
	}
	if got != rle {
		t.Errorf("AsRateLimitError returned %v, want %v", got, rle)
	}

	// A non-rate-limit error must not match.
	if _, ok := AsRateLimitError(errors.New("some other failure")); ok {
		t.Error("AsRateLimitError matched a non-rate-limit error")
	}
}
