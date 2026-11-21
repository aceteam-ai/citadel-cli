package redisapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const rateDiagnosticSecret = "synthetic-rate-capability.private"
const rateDiagnosticBody = `{"error":"synthetic-rate-capability.private","limit":123,"window":"synthetic-rate-capability.private","retry_after":17,"reset_at":"2030-01-02T03:04:05Z"}`

// Exercise actual rejected-upgrade entry points without opening a connection.
func TestMediaRateLimitDiagnosticsThroughActualWSConnect(t *testing.T) {
	for _, enable := range []bool{false, true} {
		t.Run(fmt.Sprintf("enable=%v", enable), func(t *testing.T) {
			client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture-device"})
			t.Cleanup(func() { _ = client.Close() })
			calls := 0
			client.wsClient.dialContext = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
				calls++
				return nil, &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(rateDiagnosticBody)),
				}, websocket.ErrBadHandshake
			}
			var err error
			if enable {
				err = client.EnableWebSocket(context.Background())
			} else {
				err = client.wsClient.Connect(context.Background())
			}
			rateLimit, ok := AsRateLimitError(err)
			if !ok || calls != 1 || client.wsClient.IsConnected() || client.PubSubTransport() != PubSubTransportHTTP {
				t.Fatal("audit lost the original typed metadata, backoff or failed-connection boundary")
			}
			checkOriginalRateLimitFields(t, rateLimit)
			checkRateLimitDiagnostics(t, rateLimit, err)
		})
	}
}

func checkOriginalRateLimitFields(t *testing.T, rateLimit *RateLimitError) {
	t.Helper()
	reset, err := time.Parse(time.RFC3339, "2030-01-02T03:04:05Z")
	if err != nil || rateLimit.StatusCode != 429 || rateLimit.Body != rateDiagnosticBody || rateLimit.Message != rateDiagnosticSecret ||
		rateLimit.Window != rateDiagnosticSecret || rateLimit.Limit != 123 || rateLimit.RetryAfter != 17*time.Second ||
		!rateLimit.ResetAt.Equal(reset) || rateLimit.Wait(time.Time{}) != 17*time.Second {
		t.Fatal("original typed metadata or retry hint changed")
	}
}

func checkRateLimitDiagnostics(t *testing.T, rateLimit *RateLimitError, actual error) {
	t.Helper()
	before := *rateLimit
	wrapped := fmt.Errorf("fixture wrapper: %w", actual)
	var extracted *RateLimitError
	if !errors.Is(wrapped, rateLimit) || !errors.As(wrapped, &extracted) || extracted != rateLimit {
		t.Fatal("diagnostic wrapping lost original classification or pointer identity")
	}
	var diagnostics []string
	diagnostics = append(diagnostics, actual.Error(), wrapped.Error())
	for _, value := range []any{
		rateLimit, *rateLimit, actual, wrapped,
		struct{ Err error }{actual}, struct{ Rate RateLimitError }{*rateLimit},
		map[string]RateLimitError{"rate": *rateLimit}, []*RateLimitError{rateLimit},
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			diagnostics = append(diagnostics, fmt.Sprintf(verb, value))
		}
	}
	for _, value := range []any{rateLimit, *rateLimit} {
		// A caller-controlled formatting width must not allocate padded peer
		// diagnostics; the fixed projection ignores width and precision.
		diagnostic := fmt.Sprintf("%1000000.1000000v", value)
		if len(diagnostic) > 256 {
			t.Fatal("formatter expanded caller-supplied width or precision")
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	for _, value := range []any{
		rateLimit, *rateLimit, struct{ Err error }{rateLimit},
		map[string]RateLimitError{"rate": *rateLimit}, []*RateLimitError{rateLimit},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal("diagnostic JSON could not be encoded")
		}
		diagnostics = append(diagnostics, string(encoded))
	}
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic, rateDiagnosticSecret) || strings.Contains(diagnostic, "PANIC=") {
			t.Error("ordinary rate-limit diagnostic retained peer text or panicked")
		}
	}
	encoded, err := json.Marshal(rateLimit)
	var projected map[string]json.RawMessage
	if err != nil || json.Unmarshal(encoded, &projected) != nil || len(projected) != 3 ||
		string(projected["StatusCode"]) != "429" || string(projected["Limit"]) != "123" || string(projected["RetryAfter"]) != "17000000000" {
		t.Fatal("diagnostic JSON did not retain exactly its finite numeric projection")
	}
	if *rateLimit != before {
		t.Fatal("diagnostics mutated original typed error metadata")
	}
}

func TestMediaRateLimitDiagnosticNilsAndResetLocation(t *testing.T) {
	var absent *RateLimitError
	if absent.Error() != "rate limited (details unavailable)" {
		t.Fatal("nil error did not produce its static summary")
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if diagnostic := fmt.Sprintf(verb, absent); strings.Contains(diagnostic, "PANIC=") || strings.Contains(diagnostic, rateDiagnosticSecret) {
			t.Fatal("nil formatting panicked or retained peer text")
		}
	}
	encoded, err := json.Marshal(absent)
	if err != nil || string(encoded) != "null" {
		t.Fatal("nil JSON semantics changed")
	}
	rateLimit := parseRateLimitError(429, rateDiagnosticBody)
	rateLimit.ResetAt = rateLimit.ResetAt.In(time.FixedZone(rateDiagnosticSecret, 0))
	checkRateLimitDiagnostics(t, rateLimit, rateLimit)
}

type rateDiagnosticTransport struct {
	t     *testing.T
	calls int
}

func (transport *rateDiagnosticTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.calls++
	if request.Method != http.MethodGet || request.URL.String() != "https://fixture.invalid/api/fabric/redis/ping" || request.Header.Get("Authorization") != "Bearer fixture-device" {
		transport.t.Fatal("actual HTTP ping endpoint, method or credential changed")
	}
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(rateDiagnosticBody))}, nil
}

func TestMediaRateLimitDiagnosticsThroughActualHTTPPing(t *testing.T) {
	client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture-device"})
	t.Cleanup(func() { _ = client.Close() })
	transport := &rateDiagnosticTransport{t: t}
	client.httpClient.Transport = transport
	err := client.Ping(context.Background())
	rateLimit, ok := AsRateLimitError(err)
	if !ok || transport.calls != 1 {
		t.Fatal("HTTP ping lost typed rate-limit failure or retried unexpectedly")
	}
	checkOriginalRateLimitFields(t, rateLimit)
	checkRateLimitDiagnostics(t, rateLimit, err)
}

func TestMediaRateLimitDiagnosticsThroughActualReconnectLog(t *testing.T) {
	var logs []string
	var client *WSClient
	client = NewWSClient(WSClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture-device", DebugFunc: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
		if strings.HasPrefix(format, "ws: reconnect: rate limited") {
			// Stop before the actual requested backoff sleep. No goroutine,
			// listener, connection or real wait is created by this fixture.
			_ = client.Close()
		}
	}})
	t.Cleanup(func() { _ = client.Close() })
	client.reconnectBackoff = 0
	calls := 0
	client.dialContext = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
		calls++
		return nil, &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(rateDiagnosticBody))}, websocket.ErrBadHandshake
	}
	client.reconnect()
	if calls != 1 || !strings.Contains(strings.Join(logs, "\n"), "limit=123), honoring server backoff of 17s") {
		t.Fatal("fixture did not reach the actual rate-limit retry diagnostic")
	}
	for _, diagnostic := range logs {
		if strings.Contains(diagnostic, rateDiagnosticSecret) {
			t.Error("actual reconnect log retained raw peer metadata")
		}
	}
}
