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

func newMessageTestStore(t *testing.T) *instanceStore {
	t.Helper()
	return &instanceStore{path: filepath.Join(t.TempDir(), "instances", "state.json")}
}

func newMessageJob(payload map[string]string) *nexus.Job {
	return &nexus.Job{ID: "test-job", Type: "INSTANCE_MESSAGE", Payload: payload}
}

// TestInstanceMessage_DeliversToResolvedPort verifies the handler resolves the
// host port from the store and POSTs {message,name} with the bearer to
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
		"service": "ac-abc",
		"message": "hello there",
		"name":    "Kickoff",
		"bearer":  "hooks_gw_key_123",
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
		"service": "ac-abc", "message": "hi", "bearer": "hooks_x",
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
		"service": "ac-missing", "message": "hi", "bearer": "hooks_x",
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
		{"no service", map[string]string{"message": "m", "bearer": "b"}, "service"},
		{"no message", map[string]string{"service": "ac-x", "bearer": "b"}, "message"},
		{"no bearer", map[string]string{"service": "ac-x", "message": "m"}, "bearer"},
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
		"service": "ac-abc", "message": "hi", "bearer": "hooks_x",
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
