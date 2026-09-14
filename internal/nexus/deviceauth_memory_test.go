package nexus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollForMemoryToken_ApprovesAfterPending(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/device-auth/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // memory endpoint always returns 200
		if n < 2 {
			w.Write([]byte(`{"status":"pending"}`))
			return
		}
		w.Write([]byte(`{"status":"approved","api_key":"act_minted","org_id":"org_9","org_name":"Acme","scopes":["memory:read","memory:write"],"expires_in":null}`))
	}))
	defer srv.Close()

	c := NewDeviceAuthClient(srv.URL)
	tok, err := c.pollMemory("devcode", time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("PollForMemoryToken: %v", err)
	}
	if tok.APIKey != "act_minted" {
		t.Fatalf("bad api key: %q", tok.APIKey)
	}
	if tok.OrgName != "Acme" || len(tok.Scopes) != 2 {
		t.Fatalf("bad org/scopes: %+v", tok)
	}
	if tok.ExpiresIn != nil {
		t.Fatalf("expected nil expires_in, got %v", *tok.ExpiresIn)
	}
}

func TestPollForMemoryToken_Denied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"denied"}`))
	}))
	defer srv.Close()

	c := NewDeviceAuthClient(srv.URL)
	if _, err := c.checkMemoryToken("devcode"); err != nil {
		t.Fatalf("checkMemoryToken: %v", err)
	}
	// The poll loop maps a "denied" status to an error.
	_, err := c.pollMemory("devcode", time.Millisecond, 2*time.Second)
	if err == nil {
		t.Fatal("expected denied error")
	}
}

func TestStartFlow_SendsDeviceKind(t *testing.T) {
	var sawKind string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if k, ok := body["device_kind"].(string); ok {
			sawKind = k
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"device_code":"dc","user_code":"UC","expires_in":600,"interval":1}`))
	}))
	defer srv.Close()

	c := NewDeviceAuthClient(srv.URL)
	if _, err := c.StartFlow(&StartFlowOptions{DeviceKind: "memory"}); err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	if sawKind != "memory" {
		t.Fatalf("device_kind not sent, got %q", sawKind)
	}
}
