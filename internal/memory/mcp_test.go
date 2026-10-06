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

func writeInitialize(w http.ResponseWriter, session string) {
	w.Header().Set("Content-Type", "application/json")
	if session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "test", "version": "1"},
		},
	})
}

func TestCallTool_InitializesStatelessServer(t *testing.T) {
	var gotAuth, gotTool string
	var initialized bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		method, name, _ := decodeReq(t, r)
		switch method {
		case "initialize":
			writeInitialize(w, "")
		case "notifications/initialized":
			initialized = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if !initialized {
				t.Error("tools/call arrived before initialized notification")
			}
			gotTool = name
			writeResult(w, "memory: railway uses socks5 relay")
		default:
			t.Errorf("unexpected method %q", method)
		}
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
	terminated := make(chan struct{}, 1)
	var toolCallSession string
	var initializedProtocol, toolProtocol string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != sessionID {
				t.Errorf("terminated session = %q, want %q", r.Header.Get("Mcp-Session-Id"), sessionID)
			}
			terminated <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		method, _, session := decodeReq(t, r)
		switch method {
		case "tools/call":
			toolCallSession = session
			toolProtocol = r.Header.Get("MCP-Protocol-Version")
			writeResult(w, "hello from session")
		case "initialize":
			writeInitialize(w, sessionID)
		case "notifications/initialized":
			if session != sessionID {
				t.Errorf("initialized session = %q, want %q", session, sessionID)
			}
			sawInitialized = true
			initializedProtocol = r.Header.Get("MCP-Protocol-Version")
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
	if initializedProtocol != mcpProtocolVersion || toolProtocol != mcpProtocolVersion {
		t.Fatalf("negotiated protocol header not carried: initialized=%q tool=%q", initializedProtocol, toolProtocol)
	}
	select {
	case <-terminated:
	default:
		t.Fatal("MCP session was not terminated after the tool call")
	}
}

func TestCallTool_SSEResponse(t *testing.T) {
	var initialized bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, _, _ := decodeReq(t, r)
		switch method {
		case "initialize":
			writeInitialize(w, "")
		case "notifications/initialized":
			initialized = true
			w.WriteHeader(http.StatusNoContent)
		case "tools/call":
			if !initialized {
				t.Error("tools/call arrived before initialized notification")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: message\n")
			_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"sse works"}]}}`+"\n\n")
		}
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
		method, _, _ := decodeReq(t, r)
		switch method {
		case "initialize":
			writeInitialize(w, "")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 2,
				"error": map[string]any{"code": -32000, "message": "boom"},
			})
		}
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	_, err := c.CallTool(context.Background(), "memory_search", map[string]any{"query": "x"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected tool error, got %v", err)
	}
}

func TestCallTool_ResultIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, _, _ := decodeReq(t, r)
		switch method {
		case "initialize":
			writeInitialize(w, "")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 2,
				"result": map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": "write denied"}},
				},
			})
		}
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	_, err := c.CallTool(context.Background(), "memory_write", map[string]any{"content": "x"})
	if err == nil || !strings.Contains(err.Error(), "write denied") {
		t.Fatalf("expected result.isError failure, got %v", err)
	}
}

func TestCallTool_MalformedSuccessWithoutResultFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, _, _ := decodeReq(t, r)
		switch method {
		case "initialize":
			writeInitialize(w, "")
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":2}`)
		}
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	_, err := c.CallTool(context.Background(), "memory_write", map[string]any{"content": "x"})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected malformed success to fail, got %v", err)
	}
}

func TestDecodeToolResult_RejectsEmptyObject(t *testing.T) {
	_, err := decodeToolResult(json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "no result content") {
		t.Fatalf("expected empty result object to fail, got %v", err)
	}
}

func TestCallTool_InitializedNotificationFailureStopsCall(t *testing.T) {
	var toolCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, _, _ := decodeReq(t, r)
		switch method {
		case "initialize":
			writeInitialize(w, "session")
		case "notifications/initialized":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "bad session")
		case "tools/call":
			toolCalls++
			writeResult(w, "unexpected")
		}
	}))
	defer srv.Close()

	c := NewMCPClient(srv.URL, "act_key", 2*time.Second)
	_, err := c.CallTool(context.Background(), "memory_search", nil)
	if err == nil || !strings.Contains(err.Error(), "initialized notification") {
		t.Fatalf("expected notification failure, got %v", err)
	}
	if toolCalls != 0 {
		t.Fatalf("tools/call attempted after failed initialized notification: %d", toolCalls)
	}
}

func TestParseRPCResponse_RequiresVersionAndMatchingID(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"missing id", `{"jsonrpc":"2.0","result":{}}`, "omitted id"},
		{"null id", `{"jsonrpc":"2.0","id":null,"result":{}}`, "omitted id"},
		{"wrong id", `{"jsonrpc":"2.0","id":9,"result":{}}`, "does not match"},
		{"wrong version", `{"jsonrpc":"1.0","id":2,"result":{}}`, "version"},
		{"result and error", `{"jsonrpc":"2.0","id":2,"result":{},"error":{"code":-32000,"message":"ambiguous"}}`, "exactly one"},
		{"trailing JSON", `{"jsonrpc":"2.0","id":2,"result":{}} {}`, "multiple JSON values"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRPCResponse("application/json", strings.NewReader(tc.body), 2)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestParseSSE_SkipsUnrelatedResponsesUntilMatchingID(t *testing.T) {
	body := strings.Join([]string{
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{}}`, "",
		`data: {"jsonrpc":"2.0","id":99,"result":{"content":[]}}`, "",
		`data: {"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"target"}]}}`, "",
	}, "\n")
	resp, err := parseSSE(strings.NewReader(body), 2)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeToolResult(resp.Result)
	if err != nil || out != "target" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestParseSSE_RejectsStreamWithoutMatchingID(t *testing.T) {
	_, err := parseSSE(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n"), 2)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected ID mismatch, got %v", err)
	}
}

func TestParseSSE_RejectsResponseWithResultAndError(t *testing.T) {
	body := `data: {"jsonrpc":"2.0","id":2,"result":{},"error":{"code":-32000,"message":"ambiguous"}}` + "\n\n"
	_, err := parseSSE(strings.NewReader(body), 2)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected ambiguous response rejection, got %v", err)
	}
}

func TestCallTool_RejectsUnsupportedProtocol(t *testing.T) {
	var afterInitialize int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, _, _ := decodeReq(t, r)
		if method != "initialize" {
			afterInitialize++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"protocolVersion": "2099-01-01"},
		})
	}))
	defer srv.Close()
	_, err := NewMCPClient(srv.URL, "act_key", time.Second).CallTool(context.Background(), "memory_search", nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported MCP protocol") {
		t.Fatalf("got %v", err)
	}
	if afterInitialize != 0 {
		t.Fatalf("continued after unsupported protocol: %d requests", afterInitialize)
	}
}

func TestMCPClient_RefusesRedirectWithoutLeakingBearer(t *testing.T) {
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
	_, err := NewMCPClient(redirect.URL, "act_super_secret", time.Second).CallTool(context.Background(), "memory_search", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("redirect not rejected: %v", err)
	}
	if targetAuth != "" {
		t.Fatalf("bearer leaked to redirect target: %q", targetAuth)
	}
}

func TestCallTool_RedactsCredentialFromSuccessAndErrors(t *testing.T) {
	const key = "act_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name       string
		statusCode int
		toolBody   string
		wantErr    bool
	}{
		{name: "success", statusCode: http.StatusOK, toolBody: key},
		{name: "rpc error", statusCode: http.StatusOK, toolBody: key, wantErr: true},
		{name: "http error", statusCode: http.StatusBadGateway, toolBody: key, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, _, _ := decodeReq(t, r)
				switch method {
				case "initialize":
					writeInitialize(w, "")
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				case "tools/call":
					if tc.statusCode != http.StatusOK {
						w.WriteHeader(tc.statusCode)
						_, _ = io.WriteString(w, tc.toolBody)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if tc.wantErr {
						_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "error": map[string]any{"code": -32000, "message": tc.toolBody}})
						return
					}
					writeResult(w, tc.toolBody)
				}
			}))
			defer srv.Close()

			out, err := NewMCPClient(srv.URL, key, time.Second).CallTool(context.Background(), "memory_search", nil)
			combined := out
			if err != nil {
				combined += err.Error()
			}
			if strings.Contains(combined, key) || strings.Contains(combined, "act_012345") {
				t.Fatalf("credential leaked: out=%q err=%v", out, err)
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
