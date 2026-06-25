// internal/nexus/deviceauth_test.go
package nexus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestDeviceAuthStartFlow(t *testing.T) {
	// Start mock server
	mock := StartMockDeviceAuthServer(3)
	defer mock.Close()

	// Create client
	client := NewDeviceAuthClient(mock.URL())

	// Test StartFlow
	resp, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow failed: %v", err)
	}

	// Verify response
	if resp.DeviceCode == "" {
		t.Error("DeviceCode is empty")
	}
	if resp.UserCode != "MOCK-1234" {
		t.Errorf("Expected UserCode 'MOCK-1234', got '%s'", resp.UserCode)
	}
	if resp.VerificationURI == "" {
		t.Error("VerificationURI is empty")
	}
	// Verify verification URI uses mock server URL
	expectedVerificationURI := mock.URL() + "/device"
	if resp.VerificationURI != expectedVerificationURI {
		t.Errorf("Expected VerificationURI '%s', got '%s'", expectedVerificationURI, resp.VerificationURI)
	}
	// Verify complete URI includes code parameter
	expectedCompleteURI := mock.URL() + "/device?code=MOCK-1234"
	if resp.VerificationURIComplete != expectedCompleteURI {
		t.Errorf("Expected VerificationURIComplete '%s', got '%s'", expectedCompleteURI, resp.VerificationURIComplete)
	}
	if resp.ExpiresIn != 600 {
		t.Errorf("Expected ExpiresIn 600, got %d", resp.ExpiresIn)
	}
	if resp.Interval != 1 {
		t.Errorf("Expected Interval 1, got %d", resp.Interval)
	}
}

func TestDeviceAuthPollForToken(t *testing.T) {
	// Start mock server that returns pending twice, then success
	mock := StartMockDeviceAuthServer(3)
	defer mock.Close()

	// Create client
	client := NewDeviceAuthClient(mock.URL())

	// Start flow to get device code
	resp, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow failed: %v", err)
	}

	// Test PollForToken
	startTime := time.Now()
	token, err := client.PollForToken(resp.DeviceCode, resp.Interval)
	duration := time.Since(startTime)

	if err != nil {
		t.Fatalf("PollForToken failed: %v", err)
	}

	// Verify token
	if token.Authkey == "" {
		t.Error("Authkey is empty")
	}
	if token.Authkey != "tskey-auth-mock-key-123456789" {
		t.Errorf("Expected specific authkey, got '%s'", token.Authkey)
	}

	// Verify polling happened multiple times
	pollCount := mock.GetPollCount()
	if pollCount < 3 {
		t.Errorf("Expected at least 3 polls, got %d", pollCount)
	}

	// Verify it took approximately the right amount of time
	// (2 waits of 1s between 3 polls = 2s total)
	expectedDuration := time.Duration(2) * time.Second
	if duration < expectedDuration-500*time.Millisecond {
		t.Errorf("Polling completed too quickly: %v", duration)
	}
}

func TestDeviceAuthImmediateSuccess(t *testing.T) {
	// Start mock server that returns success immediately
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()

	// Create client
	client := NewDeviceAuthClient(mock.URL())

	// Start flow
	resp, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow failed: %v", err)
	}

	// Poll for token (should succeed on first try)
	token, err := client.PollForToken(resp.DeviceCode, resp.Interval)
	if err != nil {
		t.Fatalf("PollForToken failed: %v", err)
	}

	if token.Authkey == "" {
		t.Error("Authkey is empty")
	}

	// Should only have polled once
	pollCount := mock.GetPollCount()
	if pollCount != 1 {
		t.Errorf("Expected exactly 1 poll, got %d", pollCount)
	}
}

func TestDeviceAuthClientCreation(t *testing.T) {
	client := NewDeviceAuthClient("https://example.com")
	if client == nil {
		t.Fatal("NewDeviceAuthClient returned nil")
	}
	if client.baseURL != "https://example.com" {
		t.Errorf("Expected baseURL 'https://example.com', got '%s'", client.baseURL)
	}
	if client.httpClient == nil {
		t.Error("httpClient is nil")
	}
}

func TestDeviceAuthInvalidURL(t *testing.T) {
	// Create client with invalid URL
	client := NewDeviceAuthClient("http://invalid-url-that-does-not-exist-12345.com")

	// StartFlow should fail
	_, err := client.StartFlow(nil)
	if err == nil {
		t.Error("Expected error for invalid URL, got nil")
	}
}

func TestMockServerResetPollCount(t *testing.T) {
	mock := StartMockDeviceAuthServer(2)
	defer mock.Close()

	client := NewDeviceAuthClient(mock.URL())

	// First flow
	resp, _ := client.StartFlow(nil)
	client.PollForToken(resp.DeviceCode, 1)

	count1 := mock.GetPollCount()
	if count1 < 2 {
		t.Errorf("Expected at least 2 polls, got %d", count1)
	}

	// Reset and test again
	mock.ResetPollCount()
	count2 := mock.GetPollCount()
	if count2 != 0 {
		t.Errorf("Expected poll count 0 after reset, got %d", count2)
	}
}

func TestAuthServiceURLUsedForPolling(t *testing.T) {
	// Start mock server to act as auth service
	mock := StartMockDeviceAuthServer(2)
	defer mock.Close()

	// Create client with mock server URL as auth-service
	client := NewDeviceAuthClient(mock.URL())

	// Start flow - this should hit mock server's /start endpoint
	resp, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow failed: %v", err)
	}

	// Verify the verification URI matches the auth-service URL (mock server)
	expectedVerificationURI := mock.URL() + "/device"
	if resp.VerificationURI != expectedVerificationURI {
		t.Errorf("VerificationURI should use auth-service URL. Expected '%s', got '%s'",
			expectedVerificationURI, resp.VerificationURI)
	}

	// Poll for token - this should hit mock server's /token endpoint
	token, err := client.PollForToken(resp.DeviceCode, resp.Interval)
	if err != nil {
		t.Fatalf("PollForToken failed: %v", err)
	}

	// Verify we got a token (proves polling worked against mock server)
	if token.Authkey == "" {
		t.Error("Expected authkey from polling, got empty string")
	}

	// Verify polling actually happened against the mock server
	pollCount := mock.GetPollCount()
	if pollCount < 2 {
		t.Errorf("Expected at least 2 polls against mock server, got %d", pollCount)
	}

	t.Logf("✓ Verified: Both StartFlow and PollForToken use the auth-service URL (%s)", mock.URL())
}

func TestDeviceAuthSendsHostname(t *testing.T) {
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()

	client := NewDeviceAuthClient(mock.URL())
	_, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow failed: %v", err)
	}

	hostname := mock.GetLastHostname()
	if hostname == "" {
		t.Error("Expected hostname to be sent, got empty string")
	}

	// Verify it matches actual hostname
	expected, _ := os.Hostname()
	if hostname != expected {
		t.Errorf("Expected hostname '%s', got '%s'", expected, hostname)
	}
}

func TestDeviceAuthSendsMachineID(t *testing.T) {
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()

	client := NewDeviceAuthClient(mock.URL())
	_, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow failed: %v", err)
	}

	machineID := mock.GetLastMachineID()
	if machineID == "" {
		t.Error("Expected machine_id to be sent, got empty string")
	}

	// Verify it's a valid SHA-256 hex string (64 characters)
	if len(machineID) != 64 {
		t.Errorf("Expected 64-character hex string, got %d characters: %s", len(machineID), machineID)
	}

	t.Logf("Machine ID sent: %s", machineID)
}

func TestDeviceAuthSendsForceNew(t *testing.T) {
	mock := StartMockDeviceAuthServer(1)
	defer mock.Close()

	client := NewDeviceAuthClient(mock.URL())

	// Test without force_new (default)
	_, err := client.StartFlow(nil)
	if err != nil {
		t.Fatalf("StartFlow failed: %v", err)
	}

	if mock.GetLastForceNew() {
		t.Error("Expected force_new to be false by default")
	}

	// Test with force_new = true
	_, err = client.StartFlow(&StartFlowOptions{ForceNew: true})
	if err != nil {
		t.Fatalf("StartFlow with ForceNew failed: %v", err)
	}

	if !mock.GetLastForceNew() {
		t.Error("Expected force_new to be true when ForceNew option is set")
	}
}

// --- Tests for API reachability and error classification ---

func TestCheckAPIReachable_Success(t *testing.T) {
	// Start a test server that responds to HEAD requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed) // HEAD on a POST endpoint
	}))
	defer srv.Close()

	err := CheckAPIReachable(srv.URL)
	if err != nil {
		t.Errorf("CheckAPIReachable() returned error for reachable server: %v", err)
	}
}

func TestCheckAPIReachable_EmptyURL(t *testing.T) {
	err := CheckAPIReachable("")
	if err == nil {
		t.Fatal("CheckAPIReachable(\"\") should return error")
	}
	if !errors.Is(err, ErrAPIUnreachable) {
		t.Errorf("expected ErrAPIUnreachable, got: %v", err)
	}
}

func TestCheckAPIReachable_ConnectionRefused(t *testing.T) {
	// Use a port that is almost certainly not listening
	err := CheckAPIReachable("http://127.0.0.1:1")
	if err == nil {
		t.Fatal("CheckAPIReachable should fail for refused connection")
	}
	if !errors.Is(err, ErrAPIUnreachable) {
		t.Errorf("expected ErrAPIUnreachable, got: %v", err)
	}
}

func TestCheckAPIReachable_InvalidDNS(t *testing.T) {
	err := CheckAPIReachable("http://this-host-does-not-exist-xyzzy-12345.invalid")
	if err == nil {
		t.Fatal("CheckAPIReachable should fail for unresolvable hostname")
	}
	if !errors.Is(err, ErrAPIUnreachable) {
		t.Errorf("expected ErrAPIUnreachable, got: %v", err)
	}
}

func TestClassifyNetworkError_Table(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantSentinel error
	}{
		{
			name:         "nil error",
			err:          nil,
			wantSentinel: nil,
		},
		{
			name:         "DNS error",
			err:          &net.DNSError{Err: "no such host", Name: "example.com"},
			wantSentinel: ErrAPIUnreachable,
		},
		{
			name:         "connection refused",
			err:          fmt.Errorf("dial tcp 127.0.0.1:8000: connection refused"),
			wantSentinel: ErrAPIUnreachable,
		},
		{
			name:         "context deadline exceeded",
			err:          context.DeadlineExceeded,
			wantSentinel: ErrAPIUnreachable,
		},
		{
			name:         "timeout in message",
			err:          fmt.Errorf("dial tcp 10.0.0.1:443: i/o timeout"),
			wantSentinel: ErrAPIUnreachable,
		},
		{
			name:         "TLS error",
			err:          fmt.Errorf("x509: certificate signed by unknown authority"),
			wantSentinel: ErrAPIUnreachable,
		},
		{
			name:         "generic error",
			err:          fmt.Errorf("something went wrong"),
			wantSentinel: ErrAPIUnreachable, // catch-all still wraps ErrAPIUnreachable
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := classifyNetworkError(tt.err, "http://example.com")
			if tt.wantSentinel == nil {
				if result != nil {
					t.Errorf("classifyNetworkError() = %v, want nil", result)
				}
				return
			}
			if !errors.Is(result, tt.wantSentinel) {
				t.Errorf("classifyNetworkError() = %v, want sentinel %v", result, tt.wantSentinel)
			}
		})
	}
}

func TestIsNetworkError(t *testing.T) {
	if IsNetworkError(nil) {
		t.Error("IsNetworkError(nil) should be false")
	}
	if IsNetworkError(fmt.Errorf("random error")) {
		t.Error("IsNetworkError should be false for non-network errors")
	}
	if !IsNetworkError(fmt.Errorf("wrapped: %w", ErrAPIUnreachable)) {
		t.Error("IsNetworkError should be true for wrapped ErrAPIUnreachable")
	}
}

func TestIsAuthError(t *testing.T) {
	if IsAuthError(nil) {
		t.Error("IsAuthError(nil) should be false")
	}
	if IsAuthError(fmt.Errorf("random error")) {
		t.Error("IsAuthError should be false for non-auth errors")
	}
	if !IsAuthError(fmt.Errorf("wrapped: %w", ErrTokenExpired)) {
		t.Error("IsAuthError should be true for wrapped ErrTokenExpired")
	}
}

func TestClassifyHTTPError_StatusCodes(t *testing.T) {
	tests := []struct {
		code     int
		wantAuth bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusServiceUnavailable, false},
		{http.StatusBadGateway, false},
		{http.StatusInternalServerError, false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("HTTP_%d", tt.code), func(t *testing.T) {
			err := ClassifyHTTPError(tt.code, "body")
			if err == nil {
				t.Fatal("ClassifyHTTPError should always return an error")
			}
			if tt.wantAuth && !errors.Is(err, ErrTokenExpired) {
				t.Errorf("HTTP %d should wrap ErrTokenExpired, got: %v", tt.code, err)
			}
			if !tt.wantAuth && errors.Is(err, ErrTokenExpired) {
				t.Errorf("HTTP %d should NOT wrap ErrTokenExpired, got: %v", tt.code, err)
			}
		})
	}
}

func TestStartFlow_NetworkError_Classification(t *testing.T) {
	// Create client pointing to a port that is not listening
	client := NewDeviceAuthClient("http://127.0.0.1:1")
	_, err := client.StartFlow(nil)
	if err == nil {
		t.Fatal("StartFlow should fail for unreachable server")
	}
	if !IsNetworkError(err) {
		t.Errorf("StartFlow error should be classified as network error, got: %v", err)
	}
}

func TestCheckToken_NetworkError_Classification(t *testing.T) {
	client := NewDeviceAuthClient("http://127.0.0.1:1")
	_, err := client.CheckToken("fake-device-code")
	if err == nil {
		t.Fatal("CheckToken should fail for unreachable server")
	}
	if !IsNetworkError(err) {
		t.Errorf("CheckToken error should be classified as network error, got: %v", err)
	}
}
