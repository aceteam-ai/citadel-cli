package redisapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const mediaPredecodeSecret = "synthetic-predecode.secret"

type mediaOpaqueDiagnosticError struct{}

func (mediaOpaqueDiagnosticError) Error() string {
	panic("private transport error formatter must not run")
}

type mediaTimeoutDiagnosticError struct{ mediaOpaqueDiagnosticError }

func (mediaTimeoutDiagnosticError) Timeout() bool   { return true }
func (mediaTimeoutDiagnosticError) Temporary() bool { return false }

func TestMediaWSReadErrorRetentionBeforeDecode(t *testing.T) {
	closeError := &websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: mediaPredecodeSecret}
	if !strings.Contains(closeError.Error(), mediaPredecodeSecret) {
		t.Fatal("fixture did not expose the original close reason")
	}
	for _, original := range []error{closeError, mediaOpaqueDiagnosticError{}, mediaTimeoutDiagnosticError{}} {
		var logs []string
		client := &WSClient{pongWait: 45 * time.Second, debugFunc: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		}}
		// Same helper called by readLoop, without a connection or listener.
		client.debugReadError(original)
		if len(logs) != 1 || strings.Contains(logs[0], mediaPredecodeSecret) {
			t.Fatal("pre-decode WS error detail entered diagnostics")
		}
		if _, timeout := original.(mediaTimeoutDiagnosticError); timeout {
			if !strings.Contains(logs[0], "no traffic for 45s") {
				t.Fatal("timeout diagnostic classification changed")
			}
		} else if logs[0] != "ws: read error: detail omitted" {
			t.Fatal("read failure stopped being observable")
		}
	}
	if closeError.Text != mediaPredecodeSecret {
		t.Fatal("diagnostic omission mutated the original close error")
	}
}

func TestMediaWSMessageTypeRetentionBeforeDispatch(t *testing.T) {
	for _, kind := range []string{"job", "error", "consuming", "acked", "", mediaPredecodeSecret, "job\n" + mediaPredecodeSecret} {
		var logs []string
		client := &WSClient{debugFunc: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		}}
		frame, err := json.Marshal(WSMessage{Type: kind})
		if err != nil {
			t.Fatal(err)
		}
		message, err := decodeWSDelivery(frame)
		if err != nil || message.Type != kind {
			t.Fatal("diagnostic policy changed original type decoding")
		}
		client.debugMessageType(message.Type)
		want := "unknown"
		switch kind {
		case "job", "error", "consuming", "acked":
			want = kind
		}
		if len(logs) != 1 || logs[0] != "ws: received message type="+want || strings.Contains(logs[0], mediaPredecodeSecret) {
			t.Fatal("untrusted message type entered retained diagnostics")
		}
		if message.Type != kind {
			t.Fatal("diagnostics mutated the original wire carrier")
		}
	}
}

type mediaPredecodeTransport func(*http.Request) (*http.Response, error)

func (transport mediaPredecodeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type mediaFailedBody struct{ err error }

func (body mediaFailedBody) Read([]byte) (int, error) { return 0, body.err }
func (mediaFailedBody) Close() error                 { return nil }

func checkMediaPredecodeError(t *testing.T, err, original error, context string) {
	t.Helper()
	if err == nil || errors.Unwrap(err) != original || !errors.Is(err, original) {
		t.Fatal("transport failure lost exact one-hop identity")
	}
	for _, retained := range []string{err.Error(), fmt.Sprintf("%#v", struct{ err error }{err})} {
		if strings.Contains(retained, mediaPredecodeSecret) || !strings.Contains(err.Error(), context) {
			t.Fatal("transport diagnostic leaked details or lost trusted context")
		}
	}
}

func TestMediaHTTPConsumeRequestFailureRetention(t *testing.T) {
	original := textproto.ProtocolError("malformed MIME header line: " + mediaPredecodeSecret)
	for _, path := range []string{"/api/fabric/redis/jobs/consume", "/api/fabric/worker-config"} {
		client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid"})
		client.httpClient.Transport = mediaPredecodeTransport(func(*http.Request) (*http.Response, error) {
			return nil, original
		})
		err := client.doRequest(context.Background(), http.MethodPost, path, ConsumeRequest{}, nil)
		var requestError *url.Error
		if !errors.As(err, &requestError) || requestError.Err != original {
			t.Fatal("request failure lost original transport classification")
		}
		if path == "/api/fabric/redis/jobs/consume" {
			checkMediaPredecodeError(t, err, requestError, http.MethodPost+" "+path)
		} else if !strings.Contains(err.Error(), mediaPredecodeSecret) || errors.Unwrap(err) != requestError {
			t.Fatal("unrelated route diagnostics changed")
		}
	}
}

func TestMediaHTTPConsumeBodyReadFailureRetention(t *testing.T) {
	for _, original := range []error{textproto.ProtocolError("malformed MIME header line: " + mediaPredecodeSecret), mediaOpaqueDiagnosticError{}} {
		client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid"})
		client.httpClient.Transport = mediaPredecodeTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: mediaFailedBody{err: original}}, nil
		})
		err := client.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", ConsumeRequest{}, nil)
		checkMediaPredecodeError(t, err, original, "status 503")
	}
	// Exercise the real standard-library chunked trailer parser over strings.
	// No request, network connection, DNS query or listener is created.
	wire := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n" + mediaPredecodeSecret + "\r\n\r\n"
	parse := func() *http.Response {
		response, err := http.ReadResponse(bufio.NewReader(strings.NewReader(wire)), nil)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	baseline := parse()
	_, original := io.ReadAll(baseline.Body)
	_ = baseline.Body.Close()
	var protocolError textproto.ProtocolError
	if !errors.As(original, &protocolError) || !strings.Contains(original.Error(), mediaPredecodeSecret) {
		t.Fatal("fixture did not reproduce original malformed-trailer retention")
	}
	client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid"})
	client.httpClient.Transport = mediaPredecodeTransport(func(*http.Request) (*http.Response, error) {
		return parse(), nil
	})
	err := client.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", ConsumeRequest{}, nil)
	if !errors.As(err, &protocolError) {
		t.Fatal("body read failure lost standard-library error classification")
	}
	checkMediaPredecodeError(t, err, original, "status 200")

	client.httpClient.Transport = mediaPredecodeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: mediaFailedBody{err: original}}, nil
	})
	err = client.doRequest(context.Background(), http.MethodGet, "/api/fabric/worker-config", nil, nil)
	if err == nil || errors.Unwrap(err) != original || !strings.Contains(err.Error(), mediaPredecodeSecret) {
		t.Fatal("unrelated body-read diagnostics changed")
	}
}
