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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/memory"
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

func TestMemoryBridgeAllowlistAndToolsListFiltering(t *testing.T) {
	allow := map[string]struct{}{"memory_search": {}, "memory_write": {}}
	for _, tc := range []struct {
		method, params string
		want           bool
	}{
		{"initialize", "", true}, {"ping", "", true}, {"tools/list", "", true},
		{"notifications/initialized", "", true},
		{"tools/call", `{"name":"memory_search"}`, true},
		{"tools/call", `{"name":"memory_write"}`, true},
		{"tools/call", `{"name":"list_agents"}`, false},
		{"resources/list", "", false},
	} {
		b := &mcpBridge{remoteToolAllowlist: allow}
		req := &jsonRPCRequest{Method: tc.method, Params: json.RawMessage(tc.params)}
		if got := b.memoryMethodAllowed(req); got != tc.want {
			t.Errorf("method=%s params=%s allowed=%v want=%v", tc.method, tc.params, got, tc.want)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"memory_search"},{"name":"list_agents"},{"name":"memory_write"}]}}`)
	}))
	defer srv.Close()
	var out bytes.Buffer
	b := &mcpBridge{apiKey: "key", apiURL: srv.URL, mcpServer: "aceteam", httpClient: srv.Client(), stdout: &out, remoteToolAllowlist: allow}
	b.handleToolsList(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if strings.Contains(out.String(), "list_agents") || !strings.Contains(out.String(), "memory_search") || !strings.Contains(out.String(), "memory_write") {
		t.Fatalf("memory tools/list was not filtered: %s", out.String())
	}
}

func TestMemoryBridge_EndpointCannotBeOverridden(t *testing.T) {
	cfg := &memory.Config{APIBaseURL: "https://aceteam.ai"}
	got := selectMCPEndpoint("https://attacker.invalid/mcp", cfg)
	if want := cfg.EffectiveMCPURL(); got != want {
		t.Fatalf("memory credential endpoint=%q want bound endpoint %q", got, want)
	}
	if got := selectMCPEndpoint("https://custom.test/mcp", nil); got != "https://custom.test/mcp" {
		t.Fatalf("general MCP endpoint override changed: %q", got)
	}
}

func TestMemoryBridgeValidatesBackendJSONRPCAndSSEIDs(t *testing.T) {
	for _, raw := range []string{
		`{"jsonrpc":"1.0","id":4,"result":{}}`,
		`{"jsonrpc":"2.0","result":{}}`,
		`{"jsonrpc":"2.0","id":5,"result":{}}`,
		`{"jsonrpc":"2.0","id":4,"result":{},"error":{"code":-32000,"message":"ambiguous"}}`,
	} {
		if err := validateBackendRPCResponse(json.RawMessage(raw), json.RawMessage(`4`)); err == nil {
			t.Fatalf("invalid response accepted: %s", raw)
		}
	}
	b := &mcpBridge{}
	sse := strings.NewReader(strings.Join([]string{
		"event: message", `data: {"jsonrpc":"2.0","id":99,"result":{}}`, "",
		"event: message", `data: {"jsonrpc":"2.0","id":4,"result":{"ok":true}}`, "",
	}, "\n"))
	got, err := b.parseSSEResponseExpected(sse, json.RawMessage(`4`), true)
	if err != nil || !strings.Contains(string(got), `"ok":true`) {
		t.Fatalf("matching SSE response not selected: got=%s err=%v", got, err)
	}
}

func TestMemoryBridgeRejectsAmbiguousSSEBackendResponse(t *testing.T) {
	b := &mcpBridge{}
	sse := strings.NewReader("event: message\n" +
		`data: {"jsonrpc":"2.0","id":4,"result":{},"error":{"code":-32000,"message":"ambiguous"}}` + "\n\n")
	_, err := b.parseSSEResponseExpected(sse, json.RawMessage(`4`), true)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected ambiguous SSE response rejection, got %v", err)
	}
}

func TestClassifyJSONRPCRequestID(t *testing.T) {
	for _, raw := range []string{`1`, `1.5`, `"request-1"`} {
		isNotification, err := classifyJSONRPCRequestID(json.RawMessage(raw))
		if err != nil || isNotification {
			t.Errorf("valid id %s: notification=%v err=%v", raw, isNotification, err)
		}
	}
	isNotification, err := classifyJSONRPCRequestID(nil)
	if err != nil || !isNotification {
		t.Fatalf("absent id: notification=%v err=%v", isNotification, err)
	}
	for _, raw := range []string{`null`, `true`, `{}`, `[]`} {
		if _, err := classifyJSONRPCRequestID(json.RawMessage(raw)); err == nil {
			t.Errorf("invalid id accepted: %s", raw)
		}
	}
}

func TestMCPBridgeSuppressesRequestMethodNotifications(t *testing.T) {
	backendHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
	})

	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"initialize"}`,
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"2.0","method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"memory_search"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	} {
		_, _ = io.WriteString(w, line+"\n")
	}
	_ = w.Close()

	var out bytes.Buffer
	b := &mcpBridge{
		apiKey:              "scoped-key",
		endpointURL:         server.URL,
		httpClient:          server.Client(),
		stdout:              &out,
		remoteToolAllowlist: map[string]struct{}{"memory_search": {}, "memory_write": {}},
	}
	if err := b.run(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = oldStdin
	if out.Len() != 0 {
		t.Fatalf("notifications produced responses: %s", out.String())
	}
	if backendHits != 1 {
		t.Fatalf("backend hits=%d, want only notifications/initialized forwarded", backendHits)
	}
}

func TestMCPBridge_RefusesRedirectWithoutLeakingBearer(t *testing.T) {
	var targetAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := redirect.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	b := &mcpBridge{apiKey: "top-secret", apiURL: redirect.URL, mcpServer: "aceteam", httpClient: client}
	_, err := b.forwardToBackend(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect not rejected: %v", err)
	}
	if targetAuth != "" {
		t.Fatalf("bearer leaked to redirect target: %q", targetAuth)
	}
}

func TestMemoryBridge_RedactsCredentialAndSessionFromBackend(t *testing.T) {
	const key = "act_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const session = "private-session-token"

	t.Run("success response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", session)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"\u0061\u0063\u0074\u005f0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef `+session+`"}]}}`)
		}))
		defer srv.Close()
		b := &mcpBridge{apiKey: key, endpointURL: srv.URL, httpClient: srv.Client(), remoteToolAllowlist: map[string]struct{}{"memory_search": {}}}
		got, err := b.forwardToBackend(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call"})
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatal(err)
		}
		decodedText := fmt.Sprintf("%v", decoded)
		if strings.Contains(string(got), session) || strings.Contains(decodedText, key) || strings.Contains(decodedText, session) {
			t.Fatalf("backend response leaked credential material: %s", got)
		}
	})

	t.Run("error response does not poison session", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Mcp-Session-Id", session)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, key+" "+session)
		}))
		defer srv.Close()
		b := &mcpBridge{apiKey: key, endpointURL: srv.URL, httpClient: srv.Client(), sessionID: "existing-session"}
		_, err := b.forwardToBackend(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
		if err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), session) {
			t.Fatalf("unsafe backend error: %v", err)
		}
		if b.sessionID != "existing-session" {
			t.Fatalf("error response poisoned session: %q", b.sessionID)
		}
	})
}

func TestMCPBridge_ClosesBackendSessionWithoutProtocolOutput(t *testing.T) {
	var gotMethod, gotAuth, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("Mcp-Session-Id")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	var out bytes.Buffer
	b := &mcpBridge{
		apiKey:      "act_test_key",
		endpointURL: srv.URL,
		sessionID:   "session-to-close",
		httpClient:  srv.Client(),
		stdout:      &out,
	}
	b.closeBackendSession()
	if gotMethod != http.MethodDelete || gotAuth != "Bearer act_test_key" || gotSession != "session-to-close" {
		t.Fatalf("bad close request: method=%q auth=%q session=%q", gotMethod, gotAuth, gotSession)
	}
	if b.sessionID != "" {
		t.Fatalf("closed session retained: %q", b.sessionID)
	}
	if out.Len() != 0 {
		t.Fatalf("session close wrote to MCP stdout: %s", out.String())
	}
}

func TestMemoryBridge_InvalidInitializeReleasesCandidateSession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		wantError   string
	}{
		{
			name:        "malformed JSON",
			contentType: "application/json",
			body:        `{"jsonrpc":"2.0","id":1,"result":`,
			wantError:   "decode JSON-RPC response",
		},
		{
			name:        "malformed SSE",
			contentType: "text/event-stream",
			body:        "event: message\ndata: {not-json}\n\n",
			wantError:   "decode JSON-RPC response",
		},
		{
			name:        "protocol mismatch",
			contentType: "application/json",
			body:        `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2099-01-01"}}`,
			wantError:   "protocol version",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const candidate = "candidate-session"
			var deleted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleted = append(deleted, r.Header.Get("Mcp-Session-Id"))
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Mcp-Session-Id", candidate)
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			b := &mcpBridge{
				apiKey:              "scoped-key",
				endpointURL:         srv.URL,
				httpClient:          srv.Client(),
				remoteToolAllowlist: map[string]struct{}{"memory_search": {}},
			}
			_, err := b.forwardToBackend(&jsonRPCRequest{
				JSONRPC: "2.0",
				ID:      json.RawMessage(`1`),
				Method:  "initialize",
				Params:  json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}
			if b.sessionID != "" {
				t.Fatalf("invalid initialize committed candidate session %q", b.sessionID)
			}
			if len(deleted) != 1 || deleted[0] != candidate {
				t.Fatalf("candidate session cleanup=%v, want [%s]", deleted, candidate)
			}
		})
	}
}

func TestMemoryBridge_InitializeFallbackDoesNotRetainCandidateSession(t *testing.T) {
	const candidate = "fallback-candidate-session"
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.Header.Get("Mcp-Session-Id"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Mcp-Session-Id", candidate)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	b := &mcpBridge{
		apiKey:              "scoped-key",
		endpointURL:         srv.URL,
		httpClient:          srv.Client(),
		stdout:              &out,
		remoteToolAllowlist: map[string]struct{}{"memory_search": {}},
	}
	b.handleInitialize(&jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
		Params:  json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
	})
	if b.sessionID != "" {
		t.Fatalf("fallback retained rejected candidate %q", b.sessionID)
	}
	if len(deleted) != 1 || deleted[0] != candidate {
		t.Fatalf("fallback candidate cleanup=%v, want [%s]", deleted, candidate)
	}
	var response struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("fallback response is not JSON: %q: %v", out.String(), err)
	}
	if response.Result.ProtocolVersion != mcpBridgeProtocolVersion {
		t.Fatalf("fallback protocol=%q, want %q", response.Result.ProtocolVersion, mcpBridgeProtocolVersion)
	}
}

func TestMemoryBridge_InvalidRotationRestoresPriorSession(t *testing.T) {
	const (
		prior     = "prior-session"
		candidate = "rotated-session"
	)
	var requestSession string
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.Header.Get("Mcp-Session-Id"))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		requestSession = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Mcp-Session-Id", candidate)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":99,"result":{}}`)
	}))
	defer srv.Close()

	b := &mcpBridge{
		apiKey:      "scoped-key",
		endpointURL: srv.URL,
		httpClient:  srv.Client(),
		sessionID:   prior,
	}
	_, err := b.forwardToBackend(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/list"})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected response validation error, got %v", err)
	}
	if requestSession != prior {
		t.Fatalf("request session=%q, want prior %q", requestSession, prior)
	}
	if b.sessionID != prior {
		t.Fatalf("invalid rotation replaced prior session: %q", b.sessionID)
	}
	if len(deleted) != 1 || deleted[0] != candidate {
		t.Fatalf("rotated candidate cleanup=%v, want [%s]", deleted, candidate)
	}
}

func TestMemoryBridge_ValidRotationCommitsCandidateSession(t *testing.T) {
	const candidate = "rotated-session"
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Mcp-Session-Id", candidate)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2,"result":{}}`)
	}))
	defer srv.Close()

	b := &mcpBridge{
		apiKey:              "scoped-key",
		endpointURL:         srv.URL,
		httpClient:          srv.Client(),
		sessionID:           "prior-session",
		remoteToolAllowlist: map[string]struct{}{"memory_search": {}},
	}
	if _, err := b.forwardToBackend(&jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/list"}); err != nil {
		t.Fatal(err)
	}
	if b.sessionID != candidate {
		t.Fatalf("valid rotation session=%q, want %q", b.sessionID, candidate)
	}
	if deletes != 0 {
		t.Fatalf("valid candidate was deleted %d times", deletes)
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

func TestMCPBridgeRequest202ProducesJSONRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
	})
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"memory_search","arguments":{}}}`+"\n")
	_ = w.Close()

	var out bytes.Buffer
	bridge := &mcpBridge{
		apiKey:              "scoped-key",
		endpointURL:         server.URL,
		httpClient:          server.Client(),
		stdout:              &out,
		remoteToolAllowlist: map[string]struct{}{"memory_search": {}, "memory_write": {}},
	}
	if err := bridge.run(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = oldStdin

	var resp jsonRPCResponse
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("response was not JSON-RPC: %q: %v", out.String(), err)
	}
	if string(resp.ID) != "7" || resp.Error == nil || resp.Result != nil {
		t.Fatalf("request did not receive a correlated error: %s", out.String())
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
