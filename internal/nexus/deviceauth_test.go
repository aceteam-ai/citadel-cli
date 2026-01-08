// internal/nexus/deviceauth_test.go
package nexus

import (
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
	resp, err := client.StartFlow()
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
	resp, err := client.StartFlow()
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
	resp, err := client.StartFlow()
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
	_, err := client.StartFlow()
	if err == nil {
		t.Error("Expected error for invalid URL, got nil")
	}
}

func TestMockServerResetPollCount(t *testing.T) {
	mock := StartMockDeviceAuthServer(2)
	defer mock.Close()

	client := NewDeviceAuthClient(mock.URL())

	// First flow
	resp, _ := client.StartFlow()
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
