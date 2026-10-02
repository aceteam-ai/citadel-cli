package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// mcpProtocolVersion is the MCP protocol version advertised in initialize.
const mcpProtocolVersion = "2025-06-18"

// MCPClient is a minimal streamable-HTTP MCP (JSON-RPC 2.0) client for the
// AceTeam memory endpoint. It is deliberately small: it supports exactly the
// tools/call requests the recall/capture commands need.
//
// It is "dual-mode": it first attempts a bare tools/call (works when the server
// runs statelessly), and if the server requires a session it transparently runs
// the initialize -> notifications/initialized -> tools/call handshake, carrying
// the Mcp-Session-Id header. It parses both application/json and
// text/event-stream (SSE) responses, since FastMCP streamable-HTTP may return
// either.
type MCPClient struct {
	url    string
	apiKey string
	http   *http.Client
}

// NewMCPClient builds a client for the given MCP URL and act_ bearer token.
// timeout bounds the entire call (important: recall runs on every prompt).
func NewMCPClient(url, apiKey string, timeout time.Duration) *MCPClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &MCPClient{
		url:    url,
		apiKey: apiKey,
		http:   &http.Client{Timeout: timeout},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

// toolCallResult is the subset of a tools/call result we care about.
type toolCallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// CallTool invokes an MCP tool and returns its text output (concatenated text
// content blocks). It runs the session handshake only if the server demands it.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	} else {
		params["arguments"] = map[string]any{}
	}

	// Attempt 1: stateless tools/call (no session).
	resp, _, err := c.post(ctx, "", rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: params,
	})
	if err == nil && resp.Error == nil {
		return decodeToolResult(resp.Result)
	}

	// If the failure isn't a "session required" signal, surface it.
	if err != nil && !isSessionRequired(err) {
		return "", err
	}
	if resp != nil && resp.Error != nil && !isSessionRequiredMsg(resp.Error.Message) {
		return "", resp.Error
	}

	// Attempt 2: full handshake, then retry the tools/call with the session id.
	session, err := c.initialize(ctx)
	if err != nil {
		return "", err
	}
	resp, _, err = c.post(ctx, session, rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: params,
	})
	if err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", resp.Error
	}
	return decodeToolResult(resp.Result)
}

// initialize performs initialize + notifications/initialized and returns the
// negotiated Mcp-Session-Id (may be empty if the server does not use one).
func (c *MCPClient) initialize(ctx context.Context) (string, error) {
	initResp, session, err := c.post(ctx, "", rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "citadel-memory", "version": "1"},
		},
	})
	if err != nil {
		return "", err
	}
	if initResp.Error != nil {
		return "", initResp.Error
	}
	// Best-effort initialized notification (no response expected).
	_, _, _ = c.post(ctx, session, rpcRequest{
		JSONRPC: "2.0", Method: "notifications/initialized",
	})
	return session, nil
}

// post sends one JSON-RPC message and returns the parsed response (nil for
// notifications) plus any Mcp-Session-Id the server assigned.
func (c *MCPClient) post(ctx context.Context, session string, body rpcRequest) (*rpcResponse, string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer httpResp.Body.Close()

	newSession := httpResp.Header.Get("Mcp-Session-Id")
	if newSession == "" {
		newSession = session
	}

	// A notification (no id) yields an empty/202 body.
	if body.Method == "notifications/initialized" {
		io.Copy(io.Discard, httpResp.Body)
		return nil, newSession, nil
	}

	if httpResp.StatusCode == http.StatusBadRequest {
		// Distinguish a "session required" 400 from other bad requests.
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		if isSessionRequiredMsg(string(snippet)) {
			return nil, newSession, errSessionRequired
		}
		return nil, newSession, fmt.Errorf("MCP HTTP 400: %s", strings.TrimSpace(string(snippet)))
	}
	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted {
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		return nil, newSession, fmt.Errorf("MCP HTTP %d: %s", httpResp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	resp, err := parseRPCResponse(httpResp.Header.Get("Content-Type"), httpResp.Body)
	if err != nil {
		return nil, newSession, err
	}
	return resp, newSession, nil
}

// parseRPCResponse handles both a plain JSON body and an SSE (text/event-stream)
// body, returning the first JSON-RPC message found.
func parseRPCResponse(contentType string, body io.Reader) (*rpcResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return parseSSE(body)
	}
	// Some servers omit/misreport the content type; sniff the first byte.
	buf := bufio.NewReader(body)
	first, err := buf.Peek(1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(first) > 0 && (first[0] == 'e' || first[0] == 'd' || first[0] == ':') {
		return parseSSE(buf)
	}
	var resp rpcResponse
	if err := json.NewDecoder(buf).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode MCP response: %w", err)
	}
	return &resp, nil
}

// parseSSE reads Server-Sent Events and returns the JSON parsed from the first
// "data:" line that decodes to a JSON-RPC response with a result or error.
func parseSSE(body io.Reader) (*rpcResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var last *rpcResponse
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			continue // skip non-JSON-RPC events
		}
		last = &resp
		if resp.Result != nil || resp.Error != nil {
			return &resp, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if last != nil {
		return last, nil
	}
	return nil, fmt.Errorf("no JSON-RPC message in SSE stream")
}

// decodeToolResult extracts concatenated text from a tools/call result.
func decodeToolResult(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var res toolCallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		// Fall back to returning the raw JSON so callers still get something.
		return string(raw), nil
	}
	var b strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" && c.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 && len(res.StructuredContent) > 0 {
		return string(res.StructuredContent), nil
	}
	return b.String(), nil
}

// errSessionRequired signals the server rejected a bare tools/call and needs a
// session handshake first.
var errSessionRequired = fmt.Errorf("mcp session required")

func isSessionRequired(err error) bool { return err == errSessionRequired }

func isSessionRequiredMsg(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "session")
}
