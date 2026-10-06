package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
		w.WriteHeader(http.StatusOK) // live memory records use the status-shaped response
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

func TestPollForMemoryToken_MapsSharedEndpointExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"expired_token","error_description":"expired"}`))
	}))
	defer srv.Close()

	_, err := NewDeviceAuthClient(srv.URL).pollMemory("devcode", time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "device code expired") {
		t.Fatalf("shared endpoint expiry was not mapped: %v", err)
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

func TestPollForMemoryToken_RejectsEmptyAndUnknownStatus(t *testing.T) {
	for _, statusJSON := range []string{`{}`, `{"status":"future_state"}`} {
		t.Run(statusJSON, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(statusJSON))
			}))
			defer srv.Close()
			_, err := NewDeviceAuthClient(srv.URL).pollMemory("devcode", time.Millisecond, time.Second)
			if err == nil || !strings.Contains(err.Error(), "unexpected memory authorization status") {
				t.Fatalf("status %s did not fail closed: %v", statusJSON, err)
			}
		})
	}
}

func TestPollForMemoryToken_CapsWaitToDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"pending"}`))
	}))
	defer srv.Close()

	start := time.Now()
	_, err := NewDeviceAuthClient(srv.URL).pollMemoryContext(context.Background(), "devcode", time.Hour, 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "authentication timeout") {
		t.Fatalf("expected bounded timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("server interval escaped poll budget: %s", elapsed)
	}
}

func TestMemoryPollIntervalIsBounded(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{
		{0, defaultMemoryPollInterval},
		{-1, defaultMemoryPollInterval},
		{1, time.Second},
		{31, maxMemoryPollInterval},
		{int(^uint(0) >> 1), maxMemoryPollInterval},
	} {
		if got := normalizeMemoryPollInterval(tc.seconds); got != tc.want {
			t.Errorf("normalizeMemoryPollInterval(%d)=%s want %s", tc.seconds, got, tc.want)
		}
	}
}

func TestPollForMemoryToken_ObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewDeviceAuthClient("http://127.0.0.1:1").pollMemoryContext(ctx, "devcode", time.Second, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}

func TestDeviceAuthClient_RefusesRedirect(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	_, err := NewDeviceAuthClient(redirect.URL).checkMemoryToken("sensitive-device-code")
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect was not rejected: %v", err)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("device code was replayed to redirect target (%d hits)", got)
	}
}
