package redisapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const mediaHandshakeSecret = "synthetic-upgrade-capability.private"

type mediaHandshakeOpaqueError struct {
	message string
}

func (e *mediaHandshakeOpaqueError) Error() string {
	panic(e.message)
}

func mediaHandshakeParserError(t *testing.T) error {
	t.Helper()
	// The same HTTP response parser used by the pinned gorilla dialer, with no
	// network connection, DNS lookup, pipe, listener or background goroutine.
	wire := "HTTP/1.1 503 Service Unavailable\r\n" + mediaHandshakeSecret + "\r\n\r\n"
	_, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wire)), nil)
	var protocolError textproto.ProtocolError
	if err == nil || !errors.As(err, &protocolError) || !strings.Contains(err.Error(), mediaHandshakeSecret) {
		t.Fatal("fixture did not reproduce an HTTP parser error carrying peer header text")
	}
	return err
}

func mediaHandshakeFailure(t *testing.T, client *WSClient, status int, original error) *int {
	t.Helper()
	calls := new(int)
	client.dialContext = func(ctx context.Context, target string, headers http.Header) (*websocket.Conn, *http.Response, error) {
		(*calls)++
		if ctx == nil || target != "wss://fixture.invalid/api/fabric/redis/ws" || headers.Get("Authorization") != "Bearer fixture-device" {
			t.Fatal("handshake request endpoint or authentication changed")
		}
		var response *http.Response
		if status != 0 {
			response = &http.Response{StatusCode: status, Header: make(http.Header)}
		}
		return nil, response, original
	}
	return calls
}

func checkMediaHandshakeDiagnostics(t *testing.T, err, original error, status int, logs []string) {
	t.Helper()
	if err == nil || errors.Unwrap(err) != original || !errors.Is(err, original) {
		t.Fatal("handshake failure lost the exact one-hop original error")
	}
	switch original.(type) {
	case textproto.ProtocolError:
		var protocolError textproto.ProtocolError
		if !errors.As(err, &protocolError) {
			t.Fatal("handshake failure lost HTTP parser classification")
		}
	case *mediaHandshakeOpaqueError:
		var opaque *mediaHandshakeOpaqueError
		if !errors.As(err, &opaque) || opaque != original {
			t.Fatal("handshake failure lost opaque original classification")
		}
	}
	for _, retained := range append([]string{
		err.Error(), fmt.Sprintf("%s", err), fmt.Sprintf("%v", err),
		fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", struct{ err error }{err}),
	}, logs...) {
		if strings.Contains(retained, mediaHandshakeSecret) || strings.Contains(retained, "PANIC=") {
			t.Error("handshake diagnostic retained peer details or invoked their formatter")
		}
	}
	if !strings.Contains(err.Error(), "WebSocket connection failed") {
		t.Error("handshake failure lost static connection context")
	}
	if status != 0 && !strings.Contains(err.Error(), fmt.Sprintf("status %d", status)) {
		t.Error("handshake failure lost numeric HTTP status")
	}
}

func TestMediaWSHandshakeFailurePrivacyThroughActualConnect(t *testing.T) {
	for _, status := range []int{0, http.StatusServiceUnavailable} {
		for _, opaque := range []bool{false, true} {
			for _, enable := range []bool{false, true} {
				t.Run(fmt.Sprintf("status=%d/opaque=%v/enable=%v", status, opaque, enable), func(t *testing.T) {
					original := mediaHandshakeParserError(t)
					if opaque {
						original = &mediaHandshakeOpaqueError{message: mediaHandshakeSecret}
					}
					var logs []string
					client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture-device", DebugFunc: func(format string, args ...any) {
						logs = append(logs, fmt.Sprintf(format, args...))
					}})
					t.Cleanup(func() { _ = client.Close() })
					calls := mediaHandshakeFailure(t, client.wsClient, status, original)
					var err error
					if enable {
						err = client.EnableWebSocket(context.Background())
						if err == nil || !strings.Contains(err.Error(), "failed to connect WebSocket") {
							t.Fatal("EnableWebSocket lost its existing outer failure context")
						}
						// EnableWebSocket's existing outer wrapper must remain; only
						// its inner handshake error has a one-hop original projection.
						outer := err
						err = errors.Unwrap(outer)
						if !errors.Is(outer, original) || strings.Contains(outer.Error(), mediaHandshakeSecret) || strings.Contains(fmt.Sprintf("%#v", struct{ err error }{outer}), mediaHandshakeSecret) {
							t.Error("EnableWebSocket outer diagnostics retained peer text or lost classification")
						}
					} else {
						err = client.wsClient.Connect(context.Background())
					}
					checkMediaHandshakeDiagnostics(t, err, original, status, logs)
					if *calls != 1 || client.wsClient.IsConnected() || client.wsClient.conn != nil || client.PubSubTransport() != PubSubTransportHTTP {
						t.Fatal("failed handshake changed dial count, connection state or HTTP fallback")
					}
				})
			}
		}
	}
}

func TestMediaWSHandshakeFailurePrivacyThroughActualReconnectLog(t *testing.T) {
	original := mediaHandshakeParserError(t)
	var logs []string
	var client *WSClient
	client = NewWSClient(WSClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture-device", DebugFunc: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
		if format == "ws: reconnect failed: %v" {
			// The actual reconnect logger is outside connMu. Stop the next
			// iteration after retaining this failure; no connection was opened.
			_ = client.Close()
		}
	}})
	t.Cleanup(func() { _ = client.Close() })
	client.reconnectBackoff = 0
	calls := mediaHandshakeFailure(t, client, http.StatusServiceUnavailable, original)
	client.reconnect()
	if *calls != 1 || len(logs) != 3 || !strings.Contains(logs[2], "ws: reconnect failed:") {
		t.Fatal("fixture did not reach exactly one actual reconnect failure log")
	}
	for _, retained := range logs {
		if strings.Contains(retained, mediaHandshakeSecret) {
			t.Error("actual reconnect log retained peer header text")
		}
	}
}

func TestMediaWSHandshakeRateLimitContractUnchanged(t *testing.T) {
	for _, enable := range []bool{false, true} {
		client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture-device"})
		t.Cleanup(func() { _ = client.Close() })
		const body = `{"error":"rate limit fixture","limit":123,"window":"minute","retry_after":17,"reset_at":"2030-01-02T03:04:05Z"}`
		calls := 0
		client.wsClient.dialContext = func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
			calls++
			return nil, &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, websocket.ErrBadHandshake
		}
		var err error
		if enable {
			err = client.EnableWebSocket(context.Background())
		} else {
			err = client.wsClient.Connect(context.Background())
		}
		rateLimit, ok := AsRateLimitError(err)
		reset, parseErr := time.Parse(time.RFC3339, "2030-01-02T03:04:05Z")
		if !ok || parseErr != nil || calls != 1 || rateLimit.StatusCode != 429 || rateLimit.Message != "rate limit fixture" || rateLimit.Limit != 123 || rateLimit.Window != "minute" || rateLimit.Body != body || rateLimit.Wait(time.Time{}) != 17*time.Second || !rateLimit.ResetAt.Equal(reset) || client.wsClient.IsConnected() {
			t.Fatal("typed upgrade rate-limit metadata, backoff or failed connection semantics changed")
		}
	}
}
