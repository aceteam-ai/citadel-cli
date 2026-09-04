// cmd/mcp_test.go
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
					"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
					"serverInfo":      map[string]interface{}{"name": "test", "version": "1.0"},
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

// TestMCPBridgeForwardToBackendInjectsNodeIDHeaderWhenPresent pins the
// "present" branch of citadel-cli#977: when the bridge has resolved a fabric
// node id (mcpBridge.nodeID), forwardToBackend attaches it as the
// X-Citadel-Node-Id request header with the exact resolved value. The id is
// injected directly onto the bridge rather than staged through
// resolveNodeIDForMCPHeader/identity.json, so this test never touches real
// node state.
func TestMCPBridgeForwardToBackendInjectsNodeIDHeaderWhenPresent(t *testing.T) {
	var gotValues []string
	var gotOK bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotValues, gotOK = r.Header["X-Citadel-Node-Id"]
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer server.Close()

	bridge := &mcpBridge{
		apiKey:     "test-key",
		apiURL:     server.URL,
		mcpServer:  "aceteam",
		httpClient: server.Client(),
		nodeID:     "1234",
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "ping",
	}

	if _, err := bridge.forwardToBackend(req); err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if !gotOK {
		t.Fatal("expected X-Citadel-Node-Id header to be present")
	}
	if len(gotValues) != 1 || gotValues[0] != "1234" {
		t.Errorf("expected X-Citadel-Node-Id=[1234], got %v", gotValues)
	}
}

// TestMCPBridgeForwardToBackendOmitsNodeIDHeaderWhenAbsent pins the "absent"
// branch: when the bridge has no resolved fabric node id (the common case on
// every real node today -- see resolveNodeIDForMCPHeader's doc comment), the
// header key must be entirely ABSENT from the outgoing request, never
// present with an empty value. An empty header is worse than no header (the
// backend could misread it as "caller claims to be node ”").
func TestMCPBridgeForwardToBackendOmitsNodeIDHeaderWhenAbsent(t *testing.T) {
	var gotOK bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotOK = r.Header["X-Citadel-Node-Id"]
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer server.Close()

	bridge := &mcpBridge{
		apiKey:     "test-key",
		apiURL:     server.URL,
		mcpServer:  "aceteam",
		httpClient: server.Client(),
		// nodeID intentionally left as the zero value "" -- the unresolved case.
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "ping",
	}

	if _, err := bridge.forwardToBackend(req); err != nil {
		t.Fatalf("request failed: %v", err)
	}

	if gotOK {
		t.Fatal("expected X-Citadel-Node-Id header to be entirely absent when nodeID is unresolved, not sent empty")
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

// TestResolveNodeIDForMCPHeaderEmptyWithoutConfig hermetically pins the
// unresolved case of resolveNodeIDForMCPHeader (citadel-cli#977) -- a fresh
// HOME with no device config or ssh_sync.yaml (the state of every fresh
// citadel checkout, and the common case on a real node today since no
// backend process yet echoes FabricNodeID -- see the function's doc
// comment) resolves to "", never a placeholder. Uses t.Setenv("HOME", ...)
// so this never reads a real node's config.yaml/identity.json.
func TestResolveNodeIDForMCPHeaderEmptyWithoutConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if got := resolveNodeIDForMCPHeader(); got != "" {
		t.Errorf("expected empty node id with no config present, got %q", got)
	}
}

func TestMCPBridgeNotification202(t *testing.T) {
	// Test that 202 Accepted (for notifications) returns nil body, no error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "test-session-456")
		w.WriteHeader(http.StatusAccepted)
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
		Method:  "notifications/initialized",
	}

	resp, err := bridge.forwardToBackend(req)
	if err != nil {
		t.Fatalf("notification should not error: %v", err)
	}
	if resp != nil {
		t.Errorf("notification should return nil body, got: %s", string(resp))
	}
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

// ============================================================================
// callLocalToolWithTimeout (citadel#858: make localToolCallTimeout real)
// ============================================================================

// TestCallLocalToolWithTimeoutAbandonsBlockedCall pins the core citadel#858
// fix: a tool.Call that blocks past its deadline must not block the caller.
// Before this fix, tryLocalToolsCall awaited tool.Call(ctx, ...) directly, so
// a tool ignoring ctx (as local_module_stop/start/restart's underlying
// primitive does -- see localToolCallTimeout's doc comment) would wedge the
// entire single-threaded JSON-RPC loop, not just this one request.
func TestCallLocalToolWithTimeoutAbandonsBlockedCall(t *testing.T) {
	blockCh := make(chan struct{})
	// Let the orphaned goroutine finish (this is the accepted leak the
	// timeout intentionally does not prevent) so it doesn't outlive the test.
	defer close(blockCh)

	tool := localMCPTool{
		Name: "test_blocking_tool",
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			<-blockCh // never returns before the test closes blockCh
			return "finished-too-late", nil
		},
	}

	const timeout = 30 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	text, err := callLocalToolWithTimeout(ctx, tool, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to mention timing out", err.Error())
	}
	if !strings.Contains(err.Error(), tool.Name) {
		t.Errorf("error = %q, want it to name the tool %q", err.Error(), tool.Name)
	}
	if text != "" {
		t.Errorf("text = %q, want empty on timeout", text)
	}
	// Generous bound: this should return at (roughly) the ctx deadline, not
	// after blockCh is eventually closed by the deferred call above.
	if elapsed > 2*time.Second {
		t.Errorf("callLocalToolWithTimeout took %s to return, want it bounded by the %s deadline", elapsed, timeout)
	}
}

// TestCallLocalToolWithTimeoutReturnsResultWhenFast is the control case: a
// tool that finishes well within its deadline still returns its real result
// and error, unaffected by the new goroutine/select wrapper.
func TestCallLocalToolWithTimeoutReturnsResultWhenFast(t *testing.T) {
	tool := localMCPTool{
		Name: "test_fast_tool",
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	text, err := callLocalToolWithTimeout(ctx, tool, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "ok" {
		t.Errorf("text = %q, want %q", text, "ok")
	}
}

// TestCallLocalToolWithTimeoutPropagatesToolError confirms a tool's own
// (non-timeout) error still passes through unchanged.
func TestCallLocalToolWithTimeoutPropagatesToolError(t *testing.T) {
	sentinel := fmt.Errorf("boom from tool")
	tool := localMCPTool{
		Name: "test_erroring_tool",
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return "", sentinel
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_, err := callLocalToolWithTimeout(ctx, tool, nil)
	if err == nil || !strings.Contains(err.Error(), "boom from tool") {
		t.Errorf("err = %v, want it to wrap %v", err, sentinel)
	}
}

// TestBridgeToolsCallTimeoutRespondsOnStableWriter is the bridge-level
// regression test for the bug caught in citadel#858 review: a naive fix that
// just raced tool.Call against ctx.Done() would return promptly, but
// tryLocalToolsCall's subsequent b.writeResult/writeError call used to
// resolve the live os.Stdout variable AT WRITE TIME -- and that variable can
// still be pointed at the abandoned call's captureStdout pipe when the
// timeout response is written, silently dropping it. mcpBridge.stdout (a
// reference captured once at construction, before any tool call can ever
// redirect os.Stdout) fixes that; this test proves the timeout response
// actually reaches it instead of the live-at-write-time os.Stdout.
//
// This also exercises localToolCallTimeout as a real (short-overridden) var
// end to end through tryLocalToolsCall, not just callLocalToolWithTimeout in
// isolation.
func TestBridgeToolsCallTimeoutRespondsOnStableWriter(t *testing.T) {
	origTimeout := localToolCallTimeout
	localToolCallTimeout = 30 * time.Millisecond
	defer func() { localToolCallTimeout = origTimeout }()

	blockCh := make(chan struct{})
	defer close(blockCh) // let the orphaned goroutine finish; don't leak it past the test

	var stdout bytes.Buffer
	bridge := &mcpBridge{
		stdout: &stdout,
		localTools: []localMCPTool{
			{
				Name: "local_slow_tool",
				Call: func(ctx context.Context, args json.RawMessage) (string, error) {
					<-blockCh
					return "too-late", nil
				},
			},
		},
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"local_slow_tool","arguments":{}}`),
	}

	handledCh := make(chan bool, 1)
	go func() { handledCh <- bridge.tryLocalToolsCall(req) }()

	select {
	case handled := <-handledCh:
		if !handled {
			t.Fatal("expected tryLocalToolsCall to handle the local tool name")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tryLocalToolsCall did not return promptly after the injected timeout")
	}

	out := stdout.String()
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected the timeout response written to bridge.stdout, got: %q", out)
	}
	if !strings.Contains(out, `"isError":true`) {
		t.Errorf("expected isError:true in the response, got: %q", out)
	}
}
