package redisapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type authorizationRoundTrip func(*http.Request) (*http.Response, error)

func (fn authorizationRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type authorizationBody struct {
	reader   *strings.Reader
	failRead bool
	reads    int
	closes   int
}

func (body *authorizationBody) Read(dst []byte) (int, error) {
	body.reads++
	if body.failRead {
		return 0, errors.New("private-refusal-read-error")
	}
	return body.reader.Read(dst)
}

func (body *authorizationBody) Close() error {
	body.closes++
	return nil
}

func TestAuthorizationErrorIsTypedSanitizedAndActionable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		original := &AuthorizationError{StatusCode: status}
		wrapped := fmt.Errorf("outer operation failed: %w", original)
		denied, ok := AsAuthorizationError(wrapped)
		if !ok || denied != original || denied.StatusCode != status {
			t.Fatalf("typed refusal not preserved through wrap: %v", wrapped)
		}
		for _, required := range []string{fmt.Sprintf("HTTP %d", status), "linkage/permissions not confirmed", "if enrollment is incomplete", "citadel logout && citadel init", "fresh browser approval"} {
			if !strings.Contains(original.Error(), required) {
				t.Fatalf("static refusal lacks %q: %v", required, original)
			}
		}
	}
	var nilDenial *AuthorizationError
	for _, err := range []error{nil, nilDenial, errors.New("untyped status 401"), &AuthorizationError{StatusCode: 500}, parseRateLimitError(429, `{"error":"quota"}`)} {
		if denied, ok := AsAuthorizationError(err); ok || denied != nil {
			t.Fatal("non-refusal classified from text, nil or unrelated status")
		}
	}
}

func TestHTTPAuthorizationRefusalNeverReadsOrLogsPeerBody(t *testing.T) {
	const token = "private-device-token-canary"
	const baseURL = "https://private-origin.invalid/hidden-base"
	const responseBody = `{"error":"private-device-token-canary https://private-origin.invalid/hidden-base private-response-body-canary"}`
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, operation := range []string{"ping", "worker-config", "consume"} {
			for _, failRead := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/%s/read-fails-%t", status, operation, failRead), func(t *testing.T) {
					body := &authorizationBody{reader: strings.NewReader(responseBody), failRead: failRead}
					var debug []string
					client := NewClient(ClientConfig{BaseURL: baseURL, Token: token, DebugFunc: func(format string, args ...any) {
						debug = append(debug, fmt.Sprintf(format, args...))
					}})
					calls := 0
					client.httpClient = &http.Client{Transport: authorizationRoundTrip(func(request *http.Request) (*http.Response, error) {
						calls++
						if request.Header.Get("Authorization") != "Bearer "+token {
							t.Error("fixture did not exercise authenticated request")
						}
						return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: request}, nil
					})}
					var err error
					switch operation {
					case "ping":
						err = client.Ping(context.Background())
					case "worker-config":
						_, err = client.FetchWorkerConfig(context.Background())
					case "consume":
						var response ConsumeResponse
						err = client.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", ConsumeRequest{Queue: "q", Group: "g", Consumer: "c", Count: 1}, &response)
					}
					denied, ok := AsAuthorizationError(err)
					if !ok || denied.StatusCode != status {
						t.Fatalf("HTTP refusal lost its type/status: %v", err)
					}
					if calls != 1 || body.reads != 0 || body.closes != 1 {
						t.Fatalf("refusal should close without reading/retrying: calls%d reads%d closes%d", calls, body.reads, body.closes)
					}
					for _, private := range []string{token, baseURL, "private-response-body-canary", "private-refusal-read-error", "/api/fabric/"} {
						if strings.Contains(err.Error(), private) {
							t.Fatalf("authorization error retained private response/request data: %v", err)
						}
					}
					for _, private := range []string{token, baseURL, "private-response-body-canary", "private-refusal-read-error"} {
						if strings.Contains(strings.Join(debug, "\n"), private) {
							t.Fatal("authorization response leaked through debug logging")
						}
					}
					wantConsume := 0
					if operation == "consume" {
						wantConsume = status
					}
					if client.LastConsumeStatus() != wantConsume {
						t.Fatalf("consume provenance = %d, want %d", client.LastConsumeStatus(), wantConsume)
					}
				})
			}
		}
	}
}

func TestPingRateLimitRetainsExistingTypedBackoff(t *testing.T) {
	const payload = `{"error":"quota","retry_after":42,"reset_at":"2030-01-01T00:00:00Z"}`
	body := &authorizationBody{reader: strings.NewReader(payload)}
	client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture"})
	client.httpClient = &http.Client{Transport: authorizationRoundTrip(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: body, Request: request}, nil
	})}
	err := client.Ping(context.Background())
	rate, ok := AsRateLimitError(err)
	if !ok || rate.StatusCode != 429 || rate.RetryAfter != 42*time.Second || rate.Body != payload || rate.ResetAt.IsZero() {
		t.Fatalf("429 backoff changed: %#v %v", rate, err)
	}
	if _, denied := AsAuthorizationError(err); denied || body.reads == 0 || body.closes != 1 {
		t.Fatal("429 was treated as authorization refusal or lost body handling")
	}
}

func TestConsumeStatusRecoversAndUnrelatedAuthProbeDoesNotOverwriteIt(t *testing.T) {
	client := NewClient(ClientConfig{BaseURL: "https://fixture.invalid", Token: "fixture"})
	status := http.StatusUnauthorized
	client.httpClient = &http.Client{Transport: authorizationRoundTrip(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"messages":[]}`)), Request: request}, nil
	})}
	var response ConsumeResponse
	if err := client.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", nil, &response); err == nil || client.LastConsumeStatus() != 401 {
		t.Fatalf("consume refusal provenance lost: %v status%d", err, client.LastConsumeStatus())
	}
	status = http.StatusOK
	if err := client.doRequest(context.Background(), http.MethodPost, "/api/fabric/redis/jobs/consume", nil, &response); err != nil || client.LastConsumeStatus() != 200 {
		t.Fatalf("consume recovery provenance lost: %v status%d", err, client.LastConsumeStatus())
	}
	status = http.StatusForbidden
	if err := client.Ping(context.Background()); err == nil || client.LastConsumeStatus() != 200 {
		t.Fatalf("unrelated Ping corrupted consume provenance: %v status%d", err, client.LastConsumeStatus())
	}
}
