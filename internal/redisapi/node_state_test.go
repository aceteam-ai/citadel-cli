package redisapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPostNodeState_RequestShape asserts the binary node-state post hits the
// right path with octet-stream content-type, device Bearer auth, and the exact
// body bytes — the wire-level contract the control plane depends on.
func TestPostNodeState_RequestShape(t *testing.T) {
	body := []byte{0x01, 0x02, 0x03, 0xff}

	var (
		gotPath, gotCT, gotAuth string
		gotBody                 []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "dev-token-123"})
	if err := c.PostNodeState(context.Background(), body); err != nil {
		t.Fatalf("PostNodeState: %v", err)
	}

	if gotPath != "/api/fabric/node-state" {
		t.Errorf("path = %q, want /api/fabric/node-state", gotPath)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream", gotCT)
	}
	if gotAuth != "Bearer dev-token-123" {
		t.Errorf("authorization = %q, want Bearer dev-token-123", gotAuth)
	}
	if string(gotBody) != string(body) {
		t.Errorf("body = %v, want %v", gotBody, body)
	}
}

func TestPostNodeState_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("missing device_state:write scope"))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	err := c.PostNodeState(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
}
