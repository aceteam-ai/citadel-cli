package redisapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RateLimitError is returned when the API responds with HTTP 429 (Too Many
// Requests). It carries the structured hint from the response body so callers
// can honor the server's backoff request instead of hammering the endpoint.
//
// The Redis-API 429 body looks like:
//
//	{"error":"Rate limit exceeded","limit":50000,"window":"day",
//	 "retry_after":86400,"reset_at":"2026-07-04T02:28:33Z"}
//
// Honoring retry_after/reset_at is what prevents the worker crash-loop from
// self-DoSing into the daily quota (issue #443).
type RateLimitError struct {
	// StatusCode is the HTTP status (always 429 for this type).
	StatusCode int
	// Message is the human-readable error from the body ("Rate limit exceeded").
	Message string
	// Limit is the quota ceiling (e.g. 50000), 0 if not reported.
	Limit int
	// Window is the quota window (e.g. "day"), empty if not reported.
	Window string
	// RetryAfter is the server-requested wait before retrying. Zero if the
	// body did not include a usable hint.
	RetryAfter time.Duration
	// ResetAt is when the quota resets. Zero if not reported.
	ResetAt time.Time
	// Body is the raw response body for logging/debugging.
	Body string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited (status %d): %s (retry after %s)", e.StatusCode, e.Message, e.RetryAfter)
	}
	return fmt.Sprintf("rate limited (status %d): %s", e.StatusCode, e.Message)
}

// Wait returns how long the caller should sleep before retrying, given the
// current time. It prefers RetryAfter, falling back to the remaining time until
// ResetAt. Returns 0 if no hint is available (caller should use its own
// backoff) or the window has already passed.
func (e *RateLimitError) Wait(now time.Time) time.Duration {
	if e.RetryAfter > 0 {
		return e.RetryAfter
	}
	if !e.ResetAt.IsZero() {
		if d := e.ResetAt.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// rateLimitBody mirrors the JSON shape of the 429 response.
type rateLimitBody struct {
	Error      string `json:"error"`
	Limit      int    `json:"limit"`
	Window     string `json:"window"`
	RetryAfter int64  `json:"retry_after"` // seconds
	ResetAt    string `json:"reset_at"`    // RFC3339
}

// parseRateLimitError builds a RateLimitError from a 429 response body. It
// always returns a non-nil error so callers can rely on the type even when the
// body is empty or malformed.
func parseRateLimitError(statusCode int, body string) *RateLimitError {
	e := &RateLimitError{
		StatusCode: statusCode,
		Message:    "rate limit exceeded",
		Body:       body,
	}

	var parsed rateLimitBody
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if parsed.Error != "" {
			e.Message = parsed.Error
		}
		e.Limit = parsed.Limit
		e.Window = parsed.Window
		if parsed.RetryAfter > 0 {
			e.RetryAfter = time.Duration(parsed.RetryAfter) * time.Second
		}
		if parsed.ResetAt != "" {
			if t, terr := time.Parse(time.RFC3339, parsed.ResetAt); terr == nil {
				e.ResetAt = t
			}
		}
	}

	return e
}

// AsRateLimitError extracts a *RateLimitError from an error chain, if present.
func AsRateLimitError(err error) (*RateLimitError, bool) {
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle, true
	}
	return nil, false
}
