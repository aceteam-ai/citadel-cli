package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSend_RequestContract locks in the assumed backend contract (aceteam#4219):
// exact JSON body, the two auth headers, the POST method, and the route path.
// If #4219 finalizes different field names, this test is the single place a
// reviewer reconciles them.
func TestSend_RequestContract(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotSource string
		gotCT     string
		gotBody   []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotSource = r.Header.Get(headerFabricSource)
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Token: "tok-123"})
	res, err := c.Send(context.Background(), Notification{
		Title:          "Approval needed",
		Body:           "Agent on citadel-01 wants to deploy",
		Target:         TargetChat,
		ConversationID: "conv-abc",
	})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if !res.Accepted || res.StatusCode != http.StatusOK {
		t.Fatalf("expected accepted 200, got %+v", res)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != defaultNotifyPath {
		t.Errorf("path = %q, want %q", gotPath, defaultNotifyPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok-123")
	}
	if gotSource != fabricSourceValue {
		t.Errorf("%s = %q, want %q", headerFabricSource, gotSource, fabricSourceValue)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, gotBody)
	}
	want := map[string]any{
		"title":           "Approval needed",
		"body":            "Agent on citadel-01 wants to deploy",
		"target":          "chat",
		"conversation_id": "conv-abc",
	}
	for k, v := range want {
		if sent[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, sent[k], v)
		}
	}
	// Org must NOT be in the body — it is carried by the auth token.
	if _, ok := sent["org_id"]; ok {
		t.Errorf("body unexpectedly contains org_id; org scope comes from the token")
	}
}

// TestSend_OmitsOptionalFields confirms omitempty: a minimal notification
// sends only title+body, so the JSON surface to reconcile stays small.
func TestSend_OmitsOptionalFields(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Token: "t"})
	if _, err := c.Send(context.Background(), Notification{Title: "Hi", Body: "There"}); err != nil {
		t.Fatalf("Send error: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, ok := sent["target"]; ok {
		t.Errorf("target should be omitted when empty")
	}
	if _, ok := sent["conversation_id"]; ok {
		t.Errorf("conversation_id should be omitted when empty")
	}
}

// TestSend_BackendErrorIsNotAccepted ensures a non-2xx is surfaced as an error
// and Accepted is false (so a HITL caller doesn't think a push went out).
func TestSend_BackendErrorIsNotAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("revoked"))
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, Token: "t"})
	res, err := c.Send(context.Background(), Notification{Title: "a", Body: "b"})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if res == nil || res.Accepted {
		t.Fatalf("expected not-accepted result, got %+v", res)
	}
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", res.StatusCode)
	}
}

func TestSend_Validation(t *testing.T) {
	c := NewClient(Config{BaseURL: "https://example.com", Token: "t"})
	cases := []struct {
		name string
		n    Notification
	}{
		{"missing title", Notification{Body: "b"}},
		{"missing body", Notification{Title: "a"}},
		{"blank title", Notification{Title: "   ", Body: "b"}},
	}
	for _, tc := range cases {
		if _, err := c.Send(context.Background(), tc.n); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}

	// Missing token is a clear, early error.
	noTok := NewClient(Config{BaseURL: "https://example.com"})
	if _, err := noTok.Send(context.Background(), Notification{Title: "a", Body: "b"}); err == nil {
		t.Error("expected error when token missing")
	}
}
