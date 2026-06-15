// internal/network/reauth_test.go
package network

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchFreshAuthkey_Success(t *testing.T) {
	expectedKey := "tskey-auth-fresh-key-123456"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/fabric/authkey/generate" {
			t.Errorf("expected /api/fabric/authkey/generate, got %s", r.URL.Path)
		}
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer act_test_token_123" {
			t.Errorf("expected Bearer act_test_token_123, got %s", authHeader)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(authkeyResponse{
			Authkey:   expectedKey,
			ExpiresIn: 3600,
			Message:   "Authentication key generated successfully",
		})
	}))
	defer server.Close()

	key, err := FetchFreshAuthkey(context.Background(), server.URL, "act_test_token_123")
	if err != nil {
		t.Fatalf("FetchFreshAuthkey() error = %v", err)
	}
	if key != expectedKey {
		t.Errorf("FetchFreshAuthkey() = %q, want %q", key, expectedKey)
	}
}

func TestFetchFreshAuthkey_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid API key"}`))
	}))
	defer server.Close()

	_, err := FetchFreshAuthkey(context.Background(), server.URL, "act_invalid_token")
	if err == nil {
		t.Fatal("FetchFreshAuthkey() expected error for 401 response")
	}
	t.Logf("Got expected error: %v", err)
}

func TestFetchFreshAuthkey_EmptyAuthkey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(authkeyResponse{
			Authkey: "",
		})
	}))
	defer server.Close()

	_, err := FetchFreshAuthkey(context.Background(), server.URL, "act_test_token")
	if err == nil {
		t.Fatal("FetchFreshAuthkey() expected error for empty authkey")
	}
	t.Logf("Got expected error: %v", err)
}

func TestFetchFreshAuthkey_EmptyInputs(t *testing.T) {
	ctx := context.Background()

	// Empty base URL
	_, err := FetchFreshAuthkey(ctx, "", "act_token")
	if err == nil {
		t.Error("expected error for empty apiBaseURL")
	}

	// Empty token
	_, err = FetchFreshAuthkey(ctx, "https://example.com", "")
	if err == nil {
		t.Error("expected error for empty deviceAPIToken")
	}
}

func TestFetchFreshAuthkey_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer server.Close()

	_, err := FetchFreshAuthkey(context.Background(), server.URL, "act_test_token")
	if err == nil {
		t.Fatal("FetchFreshAuthkey() expected error for 500 response")
	}
	t.Logf("Got expected error: %v", err)
}

func TestFetchFreshAuthkey_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json`))
	}))
	defer server.Close()

	_, err := FetchFreshAuthkey(context.Background(), server.URL, "act_test_token")
	if err == nil {
		t.Fatal("FetchFreshAuthkey() expected error for invalid JSON")
	}
	t.Logf("Got expected error: %v", err)
}

func TestFetchFreshAuthkey_ContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow response - the canceled context should abort first
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := FetchFreshAuthkey(ctx, server.URL, "act_test_token")
	if err == nil {
		t.Fatal("FetchFreshAuthkey() expected error for canceled context")
	}
	t.Logf("Got expected error: %v", err)
}
