package nexus

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCSRTokenPollerFallsBackOnlyForCertificateRefusal(t *testing.T) {
	for _, code := range []string{"certificate_enrollment_unavailable", "certificate_enrollment_rate_limited", "certificate_enrollment_failed", "unsupported_device_kind"} {
		t.Run(code, func(t *testing.T) {
			var requests []string
			poll := NewCSRTokenPoller("grant-a", "public-fixture-csr", func(grant, csr string) (*TokenResponse, error) {
				if grant != "grant-a" {
					t.Fatal("poll rebound its grant")
				}
				requests = append(requests, csr)
				if len(requests) == 1 {
					return nil, &TokenError{ErrorCode: code}
				}
				return &TokenResponse{Authkey: "fixture-authkey"}, nil
			})
			token, err := poll("grant-a")
			var typed *TokenError
			if token != nil || !errors.As(err, &typed) || typed.ErrorCode != "authorization_pending" {
				t.Fatal("certificate refusal aborted grant instead of retrying")
			}
			token, err = poll("grant-a")
			if err != nil || token == nil || token.Authkey != "fixture-authkey" || !reflect.DeepEqual(requests, []string{"public-fixture-csr", ""}) {
				t.Fatal("CSR fallback changed the approved grant")
			}
			if token, err = poll("grant-b"); err == nil || token != nil || len(requests) != 2 {
				t.Fatal("bound callback polled another grant")
			}
		})
	}
	for _, code := range []string{"access_denied", "expired_token", "invalid_grant", "authorization_pending", "unknown_error"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			expected := &TokenError{ErrorCode: code}
			poll := NewCSRTokenPoller("grant", "public-fixture-csr", func(_ string, csr string) (*TokenResponse, error) {
				calls++
				if csr != "public-fixture-csr" {
					t.Fatal("non-certificate error dropped CSR")
				}
				return nil, expected
			})
			for i := 0; i < 2; i++ {
				if token, err := poll("grant"); token != nil || err != expected {
					t.Fatal("authorization error was changed")
				}
			}
			if calls != 2 {
				t.Fatal("poll count changed")
			}
		})
	}
}

func TestCSRTokenPollerPreservesWireAndOmitsCSRWhenFallingBack(t *testing.T) {
	var requests []map[string]any
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/fabric/device-auth/token" || r.Method != http.MethodPost {
			t.Error("wrong token route")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid token request")
		}
		mu.Lock()
		requests = append(requests, body)
		count := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"certificate_enrollment_unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"authkey":"fixture-authkey","device_api_token":"fixture-bearer","org_id":"fixture-org"}`))
	}))
	defer server.Close()
	client := NewDeviceAuthClient(server.URL)
	poll := NewCSRTokenPoller("fixture-grant", "public-fixture-csr", client.CheckTokenWithCSR)
	_, _ = poll("fixture-grant")
	token, err := poll("fixture-grant")
	if err != nil || token == nil || token.Authkey != "fixture-authkey" || token.DeviceAPIToken != "fixture-bearer" {
		t.Fatal("legacy response changed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[0]["csr_pem"] != "public-fixture-csr" {
		t.Fatal("CSR was not carried on the real request")
	}
	if _, present := requests[1]["csr_pem"]; present {
		t.Fatal("empty fallback CSR was serialized")
	}
	for _, req := range requests {
		if req["device_code"] != "fixture-grant" || req["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Fatal("grant fields changed")
		}
	}
}

func TestCSRTokenPollerConcurrentFallbackAndGrantIsolation(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	check := func(_ string, csr string) (*TokenResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, csr)
		if csr != "" {
			return nil, &TokenError{ErrorCode: "certificate_enrollment_failed"}
		}
		return &TokenResponse{Authkey: "fixture-authkey"}, nil
	}
	poll := NewCSRTokenPoller("grant-a", "csr-a", check)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = poll("grant-a") }()
	}
	wg.Wait()
	if !reflect.DeepEqual(seen, []string{"csr-a", ""}) {
		t.Fatal("concurrent pollers duplicated CSR failure or raced fallback")
	}
	other := NewCSRTokenPoller("grant-b", "csr-b", check)
	_, _ = other("grant-b")
	if seen[2] != "csr-b" {
		t.Fatal("one grant's fallback disabled another grant's CSR")
	}
}

func TestCSRTokenPollerSerializesInFlightRequests(t *testing.T) {
	var calls atomic.Int64
	started, release, overlapped := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	poll := NewCSRTokenPoller("grant", "fixture-csr", func(_ string, _ string) (*TokenResponse, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return nil, &TokenError{ErrorCode: "certificate_enrollment_failed"}
		}
		overlapped <- struct{}{}
		return &TokenResponse{Authkey: "fixture-authkey"}, nil
	})
	done := make(chan struct{}, 2)
	go func() { _, _ = poll("grant"); done <- struct{}{} }()
	<-started
	go func() { _, _ = poll("grant"); done <- struct{}{} }()
	// A bounded probe while the first request is held. Mutex waits are not
	// durably blocked in synctest, so this intentionally uses a real timer.
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-overlapped:
		t.Error("one grant overlapped token requests")
	case <-timer.C:
	}
	close(release)
	<-done
	<-done
	if calls.Load() != 2 {
		t.Fatal("serialized second poll was lost")
	}
}
