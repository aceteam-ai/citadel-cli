// internal/nexus/deviceauth_mock.go
package nexus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
)

// MockDeviceAuthServer provides a mock HTTP server for testing device authorization flow
type MockDeviceAuthServer struct {
	server     *httptest.Server
	pollCount  int
	pollMutex  sync.Mutex
	pollsUntilSuccess int
}

// StartMockDeviceAuthServer creates and starts a mock device authorization server
// pollsUntilSuccess controls how many polls return "authorization_pending" before returning success
func StartMockDeviceAuthServer(pollsUntilSuccess int) *MockDeviceAuthServer {
	mock := &MockDeviceAuthServer{
		pollsUntilSuccess: pollsUntilSuccess,
	}

	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/fabric/device-auth/start":
			mock.handleStart(w, r)
		case "/api/fabric/device-auth/token":
			mock.handleToken(w, r)
		default:
			http.NotFound(w, r)
		}
	}))

	return mock
}

func (m *MockDeviceAuthServer) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DeviceCodeResponse{
		DeviceCode:              "mock-device-code-12345",
		UserCode:                "MOCK-1234",
		VerificationURI:         "https://aceteam.ai/device",
		VerificationURIComplete: "https://aceteam.ai/device?code=MOCK-1234",
		ExpiresIn:               600,
		Interval:                1, // Fast polling for tests
	})
}

func (m *MockDeviceAuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m.pollMutex.Lock()
	m.pollCount++
	currentCount := m.pollCount
	m.pollMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if currentCount < m.pollsUntilSuccess {
		// Return authorization_pending
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(TokenError{
			ErrorCode:        "authorization_pending",
			ErrorDescription: "User has not yet authorized the device",
		})
	} else {
		// Return success with authkey
		json.NewEncoder(w).Encode(TokenResponse{
			Authkey:   "tskey-auth-mock-key-123456789",
			ExpiresIn: 3600,
			NexusURL:  "https://nexus.aceteam.ai",
		})
	}
}

// URL returns the base URL of the mock server
func (m *MockDeviceAuthServer) URL() string {
	return m.server.URL
}

// Close shuts down the mock server
func (m *MockDeviceAuthServer) Close() {
	m.server.Close()
}

// ResetPollCount resets the poll counter (useful for testing multiple flows)
func (m *MockDeviceAuthServer) ResetPollCount() {
	m.pollMutex.Lock()
	m.pollCount = 0
	m.pollMutex.Unlock()
}

// GetPollCount returns the current poll count
func (m *MockDeviceAuthServer) GetPollCount() int {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	return m.pollCount
}
