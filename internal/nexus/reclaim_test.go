// internal/nexus/reclaim_test.go
package nexus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReclaimStaleNode_NoHostname(t *testing.T) {
	result, err := ReclaimStaleNode(context.Background(), "http://example.com", "token", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reclaimed {
		t.Error("should not reclaim when hostname is empty")
	}
}

func TestReclaimStaleNode_NoToken(t *testing.T) {
	result, err := ReclaimStaleNode(context.Background(), "http://example.com", "", "myhost")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reclaimed {
		t.Error("should not reclaim when token is empty")
	}
	if result.Message != "no API token available, skipping reclaim" {
		t.Errorf("unexpected message: %s", result.Message)
	}
}

func TestReclaimStaleNode_Success(t *testing.T) {
	var receivedHostname string
	var receivedAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/device-auth/deregister" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		receivedAuth = r.Header.Get("Authorization")

		var req DeregisterRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedHostname = req.NodeName

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(DeregisterResponse{Success: true, Message: "deleted"})
	}))
	defer server.Close()

	result, err := ReclaimStaleNode(context.Background(), server.URL, "test-token", "debian")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Reclaimed {
		t.Error("expected reclaim to succeed")
	}
	if receivedHostname != "debian" {
		t.Errorf("expected hostname 'debian', got '%s'", receivedHostname)
	}
	if receivedAuth != "Bearer test-token" {
		t.Errorf("expected 'Bearer test-token', got '%s'", receivedAuth)
	}
}

func TestReclaimStaleNode_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "Node not found"})
	}))
	defer server.Close()

	result, err := ReclaimStaleNode(context.Background(), server.URL, "test-token", "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reclaimed {
		t.Error("should not reclaim when node not found")
	}
}

func TestReclaimStaleNode_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	result, err := ReclaimStaleNode(context.Background(), server.URL, "bad-token", "myhost")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reclaimed {
		t.Error("should not reclaim on auth failure")
	}
}

func TestReclaimStaleNode_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := ReclaimStaleNode(context.Background(), server.URL, "test-token", "myhost")
	if err == nil {
		t.Error("expected error on server error")
	}
}
