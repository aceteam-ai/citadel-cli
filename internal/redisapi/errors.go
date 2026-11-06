package redisapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AuthorizationError is an HTTP 401/403 refusal, not proof that a device is
// unlinked, revoked or expired. Method and Path contain only closed diagnostic
// labels in production; no response body, origin, query or credential is kept.
type AuthorizationError struct {
	StatusCode int
	Method     string
	Path       string
}

func (e *AuthorizationError) Error() string {
	// These fields are public for typed callers. Reapply the same allowlists
	// when formatting, even if a caller constructed or changed them directly.
	context := fmt.Sprintf("%s %s request denied (HTTP %d)", authorizationMethod(e.Method), authorizationPath(e.Path), e.StatusCode)
	if e.StatusCode == http.StatusForbidden {
		return context + "; check device permissions/scopes and organization access; this response alone does not establish the cause"
	}
	if e.StatusCode == http.StatusUnauthorized {
		return context + "; device linkage/permissions not confirmed; if enrollment is incomplete, use 'citadel logout && citadel init' for fresh browser approval"
	}
	return context + "; authorization state is not confirmed"
}

func newAuthorizationError(status int, method, path string) *AuthorizationError {
	return &AuthorizationError{StatusCode: status, Method: authorizationMethod(method), Path: authorizationPath(path)}
}

func authorizationMethod(method string) string {
	switch method {
	case http.MethodGet:
		return http.MethodGet
	case http.MethodPost:
		return http.MethodPost
	case http.MethodPut:
		return http.MethodPut
	case http.MethodPatch:
		return http.MethodPatch
	case http.MethodDelete:
		return http.MethodDelete
	case http.MethodHead:
		return http.MethodHead
	case http.MethodOptions:
		return http.MethodOptions
	default:
		return "UNKNOWN"
	}
}

// authorizationPath emits only constants. Query/fragment values, URL origins
// and dynamic job IDs never become diagnostic labels, even for public fields.
func authorizationPath(path string) string {
	parsed, err := url.Parse(path)
	if err != nil || parsed.Opaque != "" || (parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "<unknown-route>"
	}
	switch parsed.Path {
	case "/api/fabric/redis/ping":
		return "/api/fabric/redis/ping"
	case "/api/fabric/worker-config":
		return "/api/fabric/worker-config"
	case "/api/fabric/redis/jobs/consume":
		return "/api/fabric/redis/jobs/consume"
	case "/api/fabric/redis/jobs/acknowledge":
		return "/api/fabric/redis/jobs/acknowledge"
	case "/api/fabric/redis/pubsub/publish":
		return "/api/fabric/redis/pubsub/publish"
	case "/api/fabric/redis/kv":
		return "/api/fabric/redis/kv"
	case "/api/fabric/redis/streams/add":
		return "/api/fabric/redis/streams/add"
	}
	const prefix = "/api/fabric/redis/finetune/"
	if strings.HasPrefix(parsed.Path, prefix) {
		id := strings.TrimPrefix(parsed.Path, prefix)
		if id != "" && !strings.Contains(id, "/") {
			return "/api/fabric/redis/finetune/{jobId}"
		}
	}
	return "<unknown-route>"
}

// AsAuthorizationError extracts a typed refusal through ordinary error wraps.
// It does not infer authorization state by parsing arbitrary error strings.
func AsAuthorizationError(err error) (*AuthorizationError, bool) {
	var denied *AuthorizationError
	if errors.As(err, &denied) && denied != nil && (denied.StatusCode == http.StatusUnauthorized || denied.StatusCode == http.StatusForbidden) {
		return denied, true
	}
	return nil, false
}

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
	// Message is the original peer error, available for explicit typed access.
	// Ordinary diagnostics intentionally omit it.
	Message string
	// Limit is the quota ceiling (e.g. 50000), 0 if not reported.
	Limit int
	// Window is the original quota window, empty if not reported. It is not a
	// trusted diagnostic label: peers can put arbitrary text here.
	Window string
	// RetryAfter is the server-requested wait before retrying. Zero if the
	// body did not include a usable hint.
	RetryAfter time.Duration
	// ResetAt is when the quota resets. Zero if not reported.
	ResetAt time.Time
	// Body is the original response for explicit typed access, not logging.
	Body string
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "rate limited (details unavailable)"
	}
	return e.diagnosticSummary()
}

func (e RateLimitError) diagnosticSummary() string {
	return fmt.Sprintf("rate limited (status %d, limit %d, retry after %s; peer details omitted)", e.StatusCode, e.Limit, e.RetryAfter)
}

// Format covers both pointer and copied-value ordinary formatting, including
// %#v. Ignore width/precision rather than allocating caller-sized padding.
// Explicit field access, aliases and reflection are not a privacy boundary.
func (e RateLimitError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.diagnosticSummary())
}

// MarshalJSON is a diagnostic projection, not a lossless rate-limit wire
// encoding. Keep the original fields and retry behavior available through the
// typed error while excluding peer text from pointer/value/container JSON.
func (e RateLimitError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		StatusCode int
		Limit      int
		RetryAfter time.Duration
	}{e.StatusCode, e.Limit, e.RetryAfter})
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

// rateLimitFromUpgrade builds a RateLimitError from a rejected WebSocket
// upgrade response (HTTP 429).
//
// The dial path is not doRequest, so without this a rate-limited handshake came
// back as an untyped fmt.Errorf and AsRateLimitError could never match it --
// meaning a retry loop would ignore retry_after and poll far tighter than the
// server asked (issue #723, the #443 failure mode). It prefers the JSON body
// (the shape the Redis-API routes emit) and falls back to the standard
// Retry-After header, which an edge proxy may send with no body at all.
func rateLimitFromUpgrade(resp *http.Response) *RateLimitError {
	body := ""
	if resp.Body != nil {
		// Bounded read: an upgrade rejection body is small, and we must not
		// block startup on a hostile/slow one.
		if b, err := io.ReadAll(io.LimitReader(resp.Body, 8192)); err == nil {
			body = string(b)
		}
		resp.Body.Close()
	}

	e := parseRateLimitError(resp.StatusCode, body)
	if e.RetryAfter <= 0 && e.ResetAt.IsZero() {
		if d, ok := parseRetryAfterHeader(resp.Header.Get("Retry-After"), time.Now()); ok {
			e.RetryAfter = d
		}
	}
	return e
}

// parseRetryAfterHeader parses an RFC 7231 Retry-After value, which is either
// delta-seconds or an HTTP-date. Returns ok=false when absent or unparseable.
func parseRetryAfterHeader(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// AsRateLimitError extracts a *RateLimitError from an error chain, if present.
func AsRateLimitError(err error) (*RateLimitError, bool) {
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle, true
	}
	return nil, false
}
