// internal/nexus/deviceauth_mock.go
package nexus

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// MockDeviceAuthServer provides a mock HTTP server for testing device authorization flow
type MockDeviceAuthServer struct {
	server            *httptest.Server
	pollCount         int
	pollMutex         sync.Mutex
	pollsUntilSuccess int
	lastHostname      string
	lastMachineID     string
	lastForceNew      bool
	// Token-request capture (citadel-cli#1062): every /token request body is
	// recorded raw so a test can assert csr_pem presence/absence and the same
	// CSR across retries without decoding assumptions.
	tokenBodies []string
	tokenCSRs   []string
	// Optional CSR-enrollment bundle echoed back on the success response. Empty
	// by default so existing tests see a byte-identical success TokenResponse.
	bundleLeaf    string
	bundleChain   string
	bundleNodeUID string
	// Optional certificate-enrollment failure (citadel-cli#1062): when set, any
	// /token request that CARRIES a csr_pem is rejected with this status and
	// {"error": code}; a request WITHOUT a csr_pem is unaffected, so a client
	// that drops its CSR and retries still reaches the normal pending/success
	// logic. Zero status ⇒ disabled.
	csrFailStatus int
	csrFailCode   string
	// Optional machine_id-keyed node_uid minting (citadel-cli#1062): when set,
	// the success response's node_uid is minted once per StartFlow machine_id
	// and reused for the same machine_id, mirroring the backend's re-enrollment
	// identity reuse so a test can prove a second login keeps the same uid.
	enrollByMachineID bool
	machineNodeUIDs   map[string]string
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

	// Decode request to capture hostname, machine ID, and force_new
	var req StartFlowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
		m.pollMutex.Lock()
		m.lastHostname = req.Hostname
		m.lastMachineID = req.MachineID
		m.lastForceNew = req.ForceNew
		m.pollMutex.Unlock()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DeviceCodeResponse{
		DeviceCode:              "mock-device-code-12345",
		UserCode:                "MOCK-1234",
		VerificationURI:         m.server.URL + "/device",
		VerificationURIComplete: m.server.URL + "/device?code=MOCK-1234",
		ExpiresIn:               600,
		Interval:                1, // Fast polling for tests
	})
}

func (m *MockDeviceAuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Capture the raw request body and any CSR before responding.
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var parsed TokenRequest
	_ = json.Unmarshal(raw, &parsed)

	m.pollMutex.Lock()
	m.pollCount++
	currentCount := m.pollCount
	m.tokenBodies = append(m.tokenBodies, string(raw))
	m.tokenCSRs = append(m.tokenCSRs, parsed.CSRPem)
	bundleLeaf, bundleChain, bundleUID := m.bundleLeaf, m.bundleChain, m.bundleNodeUID
	csrFailStatus, csrFailCode := m.csrFailStatus, m.csrFailCode
	// Resolve a machine_id-keyed node_uid for the success response when enabled.
	if m.enrollByMachineID && parsed.CSRPem != "" {
		if m.machineNodeUIDs == nil {
			m.machineNodeUIDs = map[string]string{}
		}
		uid, ok := m.machineNodeUIDs[m.lastMachineID]
		if !ok {
			uid = fmt.Sprintf("uid-%d", len(m.machineNodeUIDs)+1)
			m.machineNodeUIDs[m.lastMachineID] = uid
		}
		bundleLeaf, bundleChain, bundleUID = "LEAF", "CHAIN", uid
	}
	m.pollMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")

	// Certificate-enrollment failure: reject only CSR-bearing polls.
	if csrFailStatus != 0 && parsed.CSRPem != "" {
		w.WriteHeader(csrFailStatus)
		json.NewEncoder(w).Encode(TokenError{ErrorCode: csrFailCode})
		return
	}

	if currentCount < m.pollsUntilSuccess {
		// Return authorization_pending
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(TokenError{
			ErrorCode:        "authorization_pending",
			ErrorDescription: "User has not yet authorized the device",
		})
	} else {
		// Return success with authkey (plus a CSR-enrollment bundle when the
		// test configured one via SetEnrollmentBundle).
		json.NewEncoder(w).Encode(TokenResponse{
			Authkey:   "tskey-auth-mock-key-123456789",
			ExpiresIn: 3600,
			NexusURL:  "https://nexus.aceteam.ai",
			LeafPem:   bundleLeaf,
			ChainPem:  bundleChain,
			NodeUID:   bundleUID,
		})
	}
}

// SetEnrollmentBundle configures the leaf/chain/node_uid the mock echoes on the
// success response (citadel-cli#1062). Default empty ⇒ no bundle (legacy shape).
func (m *MockDeviceAuthServer) SetEnrollmentBundle(leafPEM, chainPEM, nodeUID string) {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	m.bundleLeaf, m.bundleChain, m.bundleNodeUID = leafPEM, chainPEM, nodeUID
}

// FailCSREnrollment makes every CSR-bearing /token request fail with the given
// HTTP status and {"error": code} body (citadel-cli#1062). A no-CSR request is
// unaffected, so a client that drops its CSR and retries still completes.
func (m *MockDeviceAuthServer) FailCSREnrollment(status int, code string) {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	m.csrFailStatus, m.csrFailCode = status, code
}

// EnrollByMachineID makes the success response mint a node_uid once per
// StartFlow machine_id and reuse it for the same machine_id (citadel-cli#1062),
// mirroring the backend's re-enrollment identity reuse so a test can prove a
// second login of the same machine keeps the same uid.
func (m *MockDeviceAuthServer) EnrollByMachineID() {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	m.enrollByMachineID = true
}

// TokenRequestBodies returns the raw JSON bodies of every /token request seen.
func (m *MockDeviceAuthServer) TokenRequestBodies() []string {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	return append([]string(nil), m.tokenBodies...)
}

// TokenRequestCSRs returns the csr_pem value from every /token request seen
// (empty string for a request that carried none).
func (m *MockDeviceAuthServer) TokenRequestCSRs() []string {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	return append([]string(nil), m.tokenCSRs...)
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

// GetLastHostname returns the hostname from the last StartFlow request
func (m *MockDeviceAuthServer) GetLastHostname() string {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	return m.lastHostname
}

// GetLastMachineID returns the machine ID from the last StartFlow request
func (m *MockDeviceAuthServer) GetLastMachineID() string {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	return m.lastMachineID
}

// GetLastForceNew returns the force_new flag from the last StartFlow request
func (m *MockDeviceAuthServer) GetLastForceNew() bool {
	m.pollMutex.Lock()
	defer m.pollMutex.Unlock()
	return m.lastForceNew
}
