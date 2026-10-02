package memory

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodeMethod reads the JSON-RPC method + name from a request body.
func decodeReq(t *testing.T, r *http.Request) (method, toolName string, sessionHdr string) {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Method, req.Params.Name, r.Header.Get("Mcp-Session-Id")
}

func writeResult(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
}

func TestCallTool_Stateless(t *testing.T) {
	var gotAuth, gotTool string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		method, name, _ := decodeReq(t, r)
		if method != "tools/call" {
			t.Errorf("stateless server got unexpected method %q", method)
		}
		gotTool = name
		writeResult(w, "memory: railway uses socks5 relay")
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	out, err := c.CallTool(context.Background(), "memory_search", map[string]any{"query": "railway"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(out, "socks5 relay") {
		t.Fatalf("bad output: %q", out)
	}
	if gotAuth != "Bearer act_key" {
		t.Fatalf("bearer not sent: %q", gotAuth)
	}
	if gotTool != "memory_search" {
		t.Fatalf("tool name not sent: %q", gotTool)
	}
}

func TestCallTool_SessionHandshake(t *testing.T) {
	const sessionID = "sess-xyz"
	var sawInitialized bool
	var toolCallSession string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, _, session := decodeReq(t, r)
		switch method {
		case "tools/call":
			if session == "" {
				// Reject bare tools/call → force handshake.
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":"Bad Request: Missing session ID"}`)
				return
			}
			toolCallSession = session
			writeResult(w, "hello from session")
		case "initialize":
			w.Header().Set("Mcp-Session-Id", sessionID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]any{"protocolVersion": mcpProtocolVersion},
			})
		case "notifications/initialized":
			sawInitialized = true
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected method %q", method)
		}
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	out, err := c.CallTool(context.Background(), "memory_search", map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(out, "hello from session") {
		t.Fatalf("bad output: %q", out)
	}
	if !sawInitialized {
		t.Fatal("initialized notification not sent")
	}
	if toolCallSession != sessionID {
		t.Fatalf("session id not carried into tools/call: %q", toolCallSession)
	}
}

func TestCallTool_SSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message\n")
		io.WriteString(w, `data: {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"sse works"}]}}`+"\n\n")
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	out, err := c.CallTool(context.Background(), "memory_search", map[string]any{"query": "x"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "sse works" {
		t.Fatalf("bad SSE output: %q", out)
	}
}

func TestCallTool_ToolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 2,
			"error": map[string]any{"code": -32000, "message": "boom"},
		})
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	_, err := c.CallTool(context.Background(), "memory_search", map[string]any{"query": "x"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected tool error, got %v", err)
	}
}
