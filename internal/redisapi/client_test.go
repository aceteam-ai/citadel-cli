package redisapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchWorkerConfig_Success(t *testing.T) {
	expected := WorkerConfigResponse{
		Queue:         "jobs:v1:gpu-general",
		ConsumerGroup: "citadel-workers",
		OrgID:         "org-123",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/worker-config" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		// Verify auth header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("unexpected auth header: %s", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Token:   "test-token",
	})

	resp, err := client.FetchWorkerConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchWorkerConfig failed: %v", err)
	}
	if resp == nil {
		t.Fatal("FetchWorkerConfig returned nil")
	}
	if resp.Queue != expected.Queue {
		t.Errorf("Queue = %q, want %q", resp.Queue, expected.Queue)
	}
	if resp.ConsumerGroup != expected.ConsumerGroup {
		t.Errorf("ConsumerGroup = %q, want %q", resp.ConsumerGroup, expected.ConsumerGroup)
	}
	if resp.OrgID != expected.OrgID {
		t.Errorf("OrgID = %q, want %q", resp.OrgID, expected.OrgID)
	}
}

func TestFetchWorkerConfig_404_ReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Token:   "test-token",
	})

	resp, err := client.FetchWorkerConfig(context.Background())
	if err != nil {
		t.Fatalf("FetchWorkerConfig should not error on 404, got: %v", err)
	}
	if resp != nil {
		t.Errorf("FetchWorkerConfig should return nil on 404, got: %+v", resp)
	}
}

func TestFetchWorkerConfig_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Token:   "test-token",
	})

	resp, err := client.FetchWorkerConfig(context.Background())
	if err == nil {
		t.Fatal("FetchWorkerConfig should error on 500")
	}
	if resp != nil {
		t.Errorf("FetchWorkerConfig should return nil on error, got: %+v", resp)
	}
}

func TestContains404(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"request failed with status 404: 404 page not found", true},
		{"API error: status 404", true},
		{"request failed with status 500: internal error", false},
		{"connection refused", false},
		{"", false},
	}

	for _, tt := range tests {
		got := contains404(tt.input)
		if got != tt.want {
			t.Errorf("contains404(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
