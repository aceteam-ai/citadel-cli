package redisapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAuthorizationContextUsesOnlyClosedLabels(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/fabric/redis/ping", "/api/fabric/redis/ping"},
		{"/api/fabric/worker-config", "/api/fabric/worker-config"},
		{"/api/fabric/redis/jobs/consume", "/api/fabric/redis/jobs/consume"},
		{"/api/fabric/redis/jobs/acknowledge", "/api/fabric/redis/jobs/acknowledge"},
		{"/api/fabric/redis/pubsub/publish", "/api/fabric/redis/pubsub/publish"},
		{"/api/fabric/redis/streams/add", "/api/fabric/redis/streams/add"},
		{"/api/fabric/redis/kv?key=private-query#private-fragment", "/api/fabric/redis/kv"},
		{"https://private-user:private-password@private-origin.invalid/api/fabric/redis/ping?private-query#private-fragment", "/api/fabric/redis/ping"},
		{"/api/fabric/redis/finetune/private-job-id?private-query#private-fragment", "/api/fabric/redis/finetune/{jobId}"},
		{"/api/fabric/redis/finetune/private-job-id/extra", "<unknown-route>"},
		{"/api/fabric/redis/finetune/", "<unknown-route>"},
		{"/private-unknown-path?private-query", "<unknown-route>"},
		{"/api/fabric/redis/ping-private-suffix", "<unknown-route>"},
		{"/api/fabric/redis/ping\nprivate-control", "<unknown-route>"},
		{"/api/fabric/redis/%zz", "<unknown-route>"},
		{"file:///api/fabric/redis/ping", "<unknown-route>"},
		{"https:private-opaque", "<unknown-route>"},
	} {
		got := newAuthorizationError(http.StatusForbidden, http.MethodGet, tc.path)
		if got.Method != http.MethodGet || got.Path != tc.want {
			t.Fatalf("route did not reduce to a closed label: got %q want %q", got.Path, tc.want)
		}
		if !strings.Contains(got.Error(), "GET "+tc.want) {
			t.Fatal("formatting discarded the safe route context")
		}
		for _, private := range []string{"private-query", "private-fragment", "private-user", "private-password", "private-origin", "private-job-id", "private-unknown", "private-control", "private-opaque"} {
			if strings.Contains(got.Error(), private) || strings.Contains(got.Path, private) {
				t.Fatal("authorization context retained untrusted route data")
			}
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions} {
		if got := newAuthorizationError(403, method, "/api/fabric/redis/ping"); got.Method != method {
			t.Fatal("allowlisted method lost its diagnostic")
		}
	}
	for _, method := range []string{"", "get", "PRIVATE-method", "GET\nprivate-control", "GET private-secret"} {
		got := newAuthorizationError(403, method, "/api/fabric/redis/ping")
		if got.Method != "UNKNOWN" || strings.Contains(got.Error(), "private") || strings.Contains(got.Error(), "PRIVATE") {
			t.Fatal("unrecognized method escaped the allowlist")
		}
	}
}

func TestAuthorizationFormattingResanitizesPublicFields(t *testing.T) {
	// Callers may mutate exported fields; Error must not trust the constructor
	// having already normalized them. Unknown values never become free text.
	for _, poisoned := range []*AuthorizationError{
		{StatusCode: 401, Method: "POST\nprivate-method", Path: "/api/fabric/redis/kv?private-query#private-fragment"},
		{StatusCode: 403, Method: "private-method", Path: "https://private-origin.invalid/api/fabric/redis/finetune/private-job"},
		{StatusCode: 403, Method: "GET", Path: "/private-path/private-token"},
	} {
		message := poisoned.Error()
		if !strings.Contains(message, fmt.Sprintf("HTTP %d", poisoned.StatusCode)) || strings.Contains(message, "private") {
			t.Fatal("public field poisoning escaped safe formatting")
		}
	}
	known := &AuthorizationError{StatusCode: 403, Method: http.MethodPost, Path: "/api/fabric/redis/pubsub/publish"}
	if !strings.Contains(known.Error(), "POST /api/fabric/redis/pubsub/publish") {
		t.Fatal("defensive formatting hid an ordinary safe diagnostic")
	}
}

func TestHTTPAuthorizationContextOmitsQueryJobIDAndRequestPayload(t *testing.T) {
	const private = "private-request-canary"
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"KV query", "/api/fabric/redis/kv?key=" + private, "/api/fabric/redis/kv"},
		{"finetune job", "/api/fabric/redis/finetune/" + private, "/api/fabric/redis/finetune/{jobId}"},
		{"unknown path", "/unknown/" + private, "<unknown-route>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var debug []string
			client := NewClient(ClientConfig{BaseURL: "https://private-base.invalid", Token: private,
				DebugFunc: func(format string, args ...any) { debug = append(debug, fmt.Sprintf(format, args...)) }})
			body := &authorizationBody{failRead: true}
			calls := 0
			client.httpClient = &http.Client{Transport: authorizationRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				if !strings.Contains(request.URL.String(), private) || request.Header.Get("Authorization") != "Bearer "+private {
					t.Fatal("fixture did not carry the private value in the actual request")
				}
				return &http.Response{StatusCode: http.StatusForbidden, Body: body, Header: make(http.Header), Request: request}, nil
			})}
			err := client.doRequest(context.Background(), http.MethodPost, tc.path, map[string]string{"private-body": private}, nil)
			denied, ok := AsAuthorizationError(err)
			if !ok || denied.Method != http.MethodPost || denied.Path != tc.want {
				t.Fatalf("HTTP refusal lost the closed context: %v", err)
			}
			if calls != 1 || body.reads != 0 || body.closes != 1 {
				t.Fatal("context diagnostics read a refusal body or retried the request")
			}
			messages := err.Error() + "\n" + strings.Join(debug, "\n")
			if strings.Contains(messages, private) || strings.Contains(messages, "private-base.invalid") || strings.Contains(messages, "private-body") {
				t.Fatal("request data leaked through authorization error or debug context")
			}
			if !strings.Contains(messages, "POST "+tc.want) || !strings.Contains(strings.Join(debug, "\n"), "body omitted") {
				t.Fatal("fixture never exercised sanitized pre-request body diagnostics")
			}
		})
	}
}
