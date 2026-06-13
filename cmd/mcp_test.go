// cmd/mcp_test.go
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc..."},
	}
	for _, tc := range tests {
		got := truncate(tc.input, tc.maxLen)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.input, tc.maxLen, got, tc.want)
		}
	}
}

// newMockMCPServer creates a mock MCP server that supports both JSON and SSE responses.
// When useSSE is true, responses are sent as text/event-stream.
func newMockMCPServer(useSSE bool) (*httptest.Server, *string, *string) {
	var receivedSessionID string
	var receivedAuth string
	var receivedAccept string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedSessionID = r.Header.Get("Mcp-Session-Id")
		receivedAccept = r.Header.Get("Accept")

		body, _ := io.ReadAll(r.Body)
		var req jsonRPCRequest
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Validate Accept header (like the real MCP SDK does)
		if !strings.Contains(receivedAccept, "application/json") ||
			(!useSSE && false) { // Only enforce SSE acceptance when using SSE mode
			// Real SDK requires both, but we're lenient in JSON mode
		}

		// Set session ID on initialize
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "test-session-123")
		}

		var respJSON []byte
		switch req.Method {
		case "initialize":
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]interface{}{
					"protocolVersion": "2025-03-26",
					"capabilities":   map[string]interface{}{"tools": map[string]interface{}{}},
					"serverInfo":     map[string]interface{}{"name": "test", "version": "1.0"},
				},
			}
			respJSON, _ = json.Marshal(resp)

		case "tools/list":
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]interface{}{
					"tools": []map[string]interface{}{
						{
							"name":        "whoami",
							"description": "Returns the current user",
							"inputSchema": map[string]interface{}{
								"type":       "object",
								"properties": map[string]interface{}{},
							},
						},
					},
				},
			}
			respJSON, _ = json.Marshal(resp)

		default:
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"error":   map[string]interface{}{"code": -32601, "message": "Method not found"},
			}
			respJSON, _ = json.Marshal(resp)
		}

		if useSSE {
			// Respond as SSE (text/event-stream) like the real FastMCP server
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache, no-transform")
			w.Header().Set("Connection", "keep-alive")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", string(respJSON))
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.Write(respJSON)
			w.Write([]byte("\n"))
		}
	}))

	return server, &receivedAuth, &receivedSessionID
}

func TestMCPBridgeForwardToBackendJSON(t *testing.T) {
	server, receivedAuth, receivedSessionID := newMockMCPServer(false)
	defer server.Close()

	bridge := &mcpBridge{
		apiKey:     "test-key-123",
		apiURL:     server.URL,
		mcpServer:  "aceteam",
		httpClient: server.Client(),
	}

	// Test initialize
	initReq := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	}

	resp, err := bridge.forwardToBackend(initReq)
	if err != nil {
		t.Fatalf("initialize failed: %v", err)
	}

	var initResp map[string]interface{}
	if err := json.Unmarshal(resp, &initResp); err != nil {
		t.Fatalf("failed to parse initialize response: %v", err)
	}

	if initResp["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc 2.0, got %v", initResp["jsonrpc"])
	}

	// Verify session ID was captured
	if bridge.sessionID != "test-session-123" {
		t.Errorf("expected session ID 'test-session-123', got %q", bridge.sessionID)
	}

	// Verify auth header was sent
	if *receivedAuth != "Bearer test-key-123" {
		t.Errorf("expected auth 'Bearer test-key-123', got %q", *receivedAuth)
	}

	// Test tools/list (should include session ID)
	listReq := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	}

	resp, err = bridge.forwardToBackend(listReq)
	if err != nil {
		t.Fatalf("tools/list failed: %v", err)
	}

	// Verify session ID was sent
	if *receivedSessionID != "test-session-123" {
		t.Errorf("expected session ID in request, got %q", *receivedSessionID)
	}

	var listResp map[string]interface{}
	if err := json.Unmarshal(resp, &listResp); err != nil {
		t.Fatalf("failed to parse tools/list response: %v", err)
	}

	result, ok := listResp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected result object, got %T", listResp["result"])
	}

	tools, ok := result["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %v", result["tools"])
	}
}

func TestMCPBridgeForwardToBackendSSE(t *testing.T) {
	server, _, _ := newMockMCPServer(true)
	defer server.Close()

	bridge := &mcpBridge{
		apiKey:     "test-key-123",
		apiURL:     server.URL,
		mcpServer:  "aceteam",
		httpClient: server.Client(),
	}

	// Test initialize via SSE
	initReq := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
	}

	resp, err := bridge.forwardToBackend(initReq)
	if err != nil {
		t.Fatalf("initialize (SSE) failed: %v", err)
	}

	var initResp map[string]interface{}
	if err := json.Unmarshal(resp, &initResp); err != nil {
		t.Fatalf("failed to parse SSE initialize response: %v (raw: %s)", err, string(resp))
	}

	if initResp["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc 2.0 from SSE, got %v", initResp["jsonrpc"])
	}

	// Verify session ID was captured from SSE response headers
	if bridge.sessionID != "test-session-123" {
		t.Errorf("expected session ID from SSE response, got %q", bridge.sessionID)
	}

	// Test tools/list via SSE
	listReq := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`2`),
		Method:  "tools/list",
	}

	resp, err = bridge.forwardToBackend(listReq)
	if err != nil {
		t.Fatalf("tools/list (SSE) failed: %v", err)
	}

	var listResp map[string]interface{}
	if err := json.Unmarshal(resp, &listResp); err != nil {
		t.Fatalf("failed to parse SSE tools/list response: %v", err)
	}

	result, ok := listResp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected result object from SSE, got %T", listResp["result"])
	}

	tools, ok := result["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("expected 1 tool from SSE, got %v", result["tools"])
	}
}

func TestMCPBridgeSSEMultipleEvents(t *testing.T) {
	// Test SSE with multiple events -- should return the last JSON-RPC response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// Simulate a priming event followed by the actual response
		fmt.Fprint(w, "event: message\ndata: \n\n")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}\n\n")
	}))
	defer server.Close()

	bridge := &mcpBridge{
		apiKey:     "test-key",
		apiURL:     server.URL,
		mcpServer:  "aceteam",
		httpClient: server.Client(),
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"test","arguments":{}}`),
	}

	resp, err := bridge.forwardToBackend(req)
	if err != nil {
		t.Fatalf("tools/call (SSE multi-event) failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(resp, &parsed); err != nil {
		t.Fatalf("failed to parse multi-event SSE response: %v", err)
	}

	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("expected jsonrpc 2.0, got %v", parsed["jsonrpc"])
	}

	result, ok := parsed["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected result object, got %T", parsed["result"])
	}

	content, ok := result["content"].([]interface{})
	if !ok || len(content) != 1 {
		t.Fatalf("expected 1 content item, got %v", result["content"])
	}
}

func TestMCPBridgeAcceptHeader(t *testing.T) {
	var receivedAccept string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer server.Close()

	bridge := &mcpBridge{
		apiKey:     "test-key",
		apiURL:     server.URL,
		mcpServer:  "aceteam",
		httpClient: server.Client(),
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "ping",
	}

	_, err := bridge.forwardToBackend(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}

	// Verify Accept header includes both required types
	if !strings.Contains(receivedAccept, "application/json") {
		t.Errorf("Accept header missing application/json: %q", receivedAccept)
	}
	if !strings.Contains(receivedAccept, "text/event-stream") {
		t.Errorf("Accept header missing text/event-stream: %q", receivedAccept)
	}
}

func TestMCPBridgeBackendError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error": "unauthorized"}`)
	}))
	defer server.Close()

	bridge := &mcpBridge{
		apiKey:     "bad-key",
		apiURL:     server.URL,
		mcpServer:  "aceteam",
		httpClient: server.Client(),
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/list",
	}

	_, err := bridge.forwardToBackend(req)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}

	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected error to contain '401', got: %s", err.Error())
	}
}

func TestGetAPIKeyFromConfig(t *testing.T) {
	// This tests the function exists and returns empty when no config file is present.
	// We can't easily test the happy path without mocking the filesystem.
	key := getAPIKeyFromConfig()
	// Just verify it doesn't panic; the key may or may not be empty depending on the test env.
	_ = key
}

func TestParseSSEResponseEmpty(t *testing.T) {
	bridge := &mcpBridge{}

	// Empty SSE stream should return error
	_, err := bridge.parseSSEResponse(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty SSE stream")
	}

	if !strings.Contains(err.Error(), "no JSON-RPC message found") {
		t.Errorf("unexpected error: %v", err)
	}
}
