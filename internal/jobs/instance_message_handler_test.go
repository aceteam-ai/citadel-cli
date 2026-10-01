// internal/jobs/instance_message_handler_test.go
package jobs

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

const (
	messageTokenA = "123e4567-e89b-42d3-a456-426614174000"
	messageTokenB = "123e4567-e89b-42d3-a456-426614174001"
)

func newMessageTestStore(t *testing.T) *instanceStore {
	t.Helper()
	return &instanceStore{path: filepath.Join(t.TempDir(), "instances", "state.json")}
}

func newMessageJob(payload map[string]string) *nexus.Job {
	return &nexus.Job{ID: "test-job", Type: "INSTANCE_MESSAGE", Payload: payload}
}

// TestInstanceMessage_DeliversToResolvedPort verifies the handler resolves the
// host port from the store and POSTs {message,name,turnToken} with the bearer to
// <base>/hooks/agent.
func TestInstanceMessage_DeliversToResolvedPort(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newMessageTestStore(t)
	if err := store.Put(InstanceRecord{ServiceName: "ac-abc", HostPort: 18800, ContainerName: "citadel-ac-abc"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var gotPort int
	h := &InstanceMessageHandler{
		instances: store,
		loopbackBaseURL: func(port int) string {
			gotPort = port
			return srv.URL
		},
	}

	out, err := h.Execute(JobContext{}, newMessageJob(map[string]string{
		"service":   "ac-abc",
		"message":   "hello there",
		"name":      "Kickoff",
		"bearer":    "hooks_gw_key_123",
		"turnToken": messageTokenA,
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if gotPort != 18800 {
		t.Errorf("resolved port = %d, want 18800", gotPort)
	}
	if gotPath != "/hooks/agent" {
		t.Errorf("path = %q, want /hooks/agent", gotPath)
	}
	if gotAuth != "Bearer hooks_gw_key_123" {
		t.Errorf("auth = %q, want Bearer hooks_gw_key_123", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if gotBody["message"] != "hello there" || gotBody["name"] != "Kickoff" {
		t.Errorf("body = %+v, want message=hello there name=Kickoff", gotBody)
	}
	if gotBody["turnToken"] != messageTokenA {
		t.Errorf("turn token = %q, want exact inbound token", gotBody["turnToken"])
	}

	var res instanceMessageResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !res.Delivered || res.Service != "ac-abc" || res.Status != http.StatusOK {
		t.Errorf("result = %+v, want delivered=true service=ac-abc status=200", res)
	}
}

// TestInstanceMessage_DefaultsName verifies the name defaults to Coordination.
func TestInstanceMessage_DefaultsName(t *testing.T) {
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotName = body["name"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newMessageTestStore(t)
	_ = store.Put(InstanceRecord{ServiceName: "ac-abc", HostPort: 20000})
	h := &InstanceMessageHandler{instances: store, loopbackBaseURL: func(int) string { return srv.URL }}

	if _, err := h.Execute(JobContext{}, newMessageJob(map[string]string{
		"service": "ac-abc", "message": "hi", "bearer": "hooks_x", "turnToken": messageTokenA,
	})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotName != "Coordination" {
		t.Errorf("default name = %q, want Coordination", gotName)
	}
}

// TestInstanceMessage_FailsClosedOnUnknownInstance verifies that a service not
// present in the store is rejected WITHOUT any HTTP call (no mis-delivery).
func TestInstanceMessage_FailsClosedOnUnknownInstance(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := newMessageTestStore(t) // empty
	h := &InstanceMessageHandler{instances: store, loopbackBaseURL: func(int) string { return srv.URL }}

	_, err := h.Execute(JobContext{}, newMessageJob(map[string]string{
		"service": "ac-missing", "message": "hi", "bearer": "hooks_x", "turnToken": messageTokenA,
	}))
	if err == nil {
		t.Fatal("expected error for unknown instance, got nil")
	}
	if !strings.Contains(err.Error(), "not a known running instance") {
		t.Errorf("error = %v, want fail-closed unknown-instance error", err)
	}
	if called {
		t.Error("handler POSTed for an unknown instance; must fail closed without delivery")
	}
}

// TestInstanceMessage_MissingFields verifies payload validation.
func TestInstanceMessage_MissingFields(t *testing.T) {
	h := &InstanceMessageHandler{instances: newMessageTestStore(t)}
	cases := []struct {
		name    string
		payload map[string]string
		want    string
	}{
		{"no service", map[string]string{"message": "m", "bearer": "b", "turnToken": messageTokenA}, "service"},
		{"no message", map[string]string{"service": "ac-x", "bearer": "b", "turnToken": messageTokenA}, "message"},
		{"no bearer", map[string]string{"service": "ac-x", "message": "m", "turnToken": messageTokenA}, "bearer"},
		{"no turn token", map[string]string{"service": "ac-x", "message": "m", "bearer": "b"}, "turn token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Execute(JobContext{}, newMessageJob(tc.payload))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestInstanceMessage_InvalidTokenDoesNotDeliver(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	store := newMessageTestStore(t)
	if err := store.Put(InstanceRecord{ServiceName: "ac-abc", InstanceID: "instance-secret", HostPort: 20000}); err != nil {
		t.Fatal(err)
	}
	h := &InstanceMessageHandler{instances: store, loopbackBaseURL: func(int) string { return srv.URL }}
	for _, token := range []string{"", "sensitive-malformed-token", "123E4567-E89B-42D3-A456-426614174000", "123e4567e89b42d3a456426614174000", "00000000-0000-0000-0000-000000000000", "123e4567-e89b-92d3-a456-426614174000"} {
		_, err := h.Execute(JobContext{}, newMessageJob(map[string]string{
			"service": "ac-abc", "message": "sensitive-message", "bearer": "hooks_secret-key", "turnToken": token,
		}))
		if err == nil || err.Error() != "job payload invalid turn token" {
			t.Fatalf("invalid-token diagnostic = %v", err)
		}
		for _, secret := range []string{token, "sensitive-message", "hooks_secret-key", "instance-secret"} {
			if secret != "" && strings.Contains(err.Error(), secret) {
				t.Fatalf("diagnostic exposed sensitive value: %q", err)
			}
		}
	}
	if posts != 0 {
		t.Fatalf("delivered %d tokenless turns", posts)
	}
}

func TestInstanceMessage_RetryPreservesEachTurnToken(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		tokens = append(tokens, body["turnToken"])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	store := newMessageTestStore(t)
	if err := store.Put(InstanceRecord{ServiceName: "ac-abc", InstanceID: "same-instance", HostPort: 20000}); err != nil {
		t.Fatal(err)
	}
	h := &InstanceMessageHandler{instances: store, loopbackBaseURL: func(int) string { return srv.URL }}
	for _, token := range []string{messageTokenA, messageTokenB} {
		_, err := h.Execute(JobContext{}, newMessageJob(map[string]string{
			"service": "ac-abc", "message": "hi", "bearer": "hooks_x", "turnToken": token,
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(tokens) != 2 || tokens[0] != messageTokenA || tokens[1] != messageTokenB {
		t.Fatalf("delivered tokens = %v", tokens)
	}
}

// TestInstanceMessage_PropagatesHooks4xx verifies a 4xx from the container is
// surfaced as a delivery error (so the platform learns the turn was rejected).
func TestInstanceMessage_PropagatesHooks4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	store := newMessageTestStore(t)
	_ = store.Put(InstanceRecord{ServiceName: "ac-abc", HostPort: 20001})
	h := &InstanceMessageHandler{instances: store, loopbackBaseURL: func(int) string { return srv.URL }}

	_, err := h.Execute(JobContext{}, newMessageJob(map[string]string{
		"service": "ac-abc", "message": "hi", "bearer": "hooks_x", "turnToken": messageTokenA,
	}))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want status 401 surfaced", err)
	}
}

// TestHooksAgentURL verifies pure URL construction.
func TestHooksAgentURL(t *testing.T) {
	if got := hooksAgentURL(defaultLoopbackBaseURL(18800)); got != "http://127.0.0.1:18800/hooks/agent" {
		t.Errorf("hooksAgentURL = %q", got)
	}
	if got := defaultLoopbackBaseURL(9999); got != fmt.Sprintf("http://127.0.0.1:%d", 9999) {
		t.Errorf("defaultLoopbackBaseURL = %q", got)
	}
}
