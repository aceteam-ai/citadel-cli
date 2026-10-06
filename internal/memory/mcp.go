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

const maxMCPResponseBytes = 4 << 20

// MCPClient is a minimal streamable-HTTP MCP (JSON-RPC 2.0) client for the
// AceTeam memory endpoint. It is deliberately small: it supports exactly the
// tools/call requests the recall/capture commands need.
//
// Every call performs the MCP initialize -> notifications/initialized ->
// tools/call handshake. Streamable HTTP requires initialization even when the
// server elects not to issue a session id. It parses both application/json and
// text/event-stream (SSE) responses, since FastMCP may return either.
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
		http: &http.Client{
			Timeout: timeout,
			// Never replay the bearer to a redirect target. Endpoint moves must
			// be explicit in the trusted local configuration.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
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
// content blocks) after a conformant MCP handshake.
func (c *MCPClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	} else {
		params["arguments"] = map[string]any{}
	}

	session, protocolVersion, err := c.initialize(ctx)
	if err != nil {
		return "", c.safeError(fmt.Errorf("initialize MCP session: %w", err), session)
	}
	defer func() { c.terminateSession(session, protocolVersion) }()
	resp, activeSession, err := c.post(ctx, session, protocolVersion, rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: params,
	})
	if activeSession != "" {
		session = activeSession
	}
	if err != nil {
		return "", c.safeError(err, session)
	}
	if resp == nil {
		return "", fmt.Errorf("empty MCP tools/call response")
	}
	if resp.Error != nil {
		return "", c.safeError(resp.Error, session)
	}
	text, err := decodeToolResult(resp.Result, c.apiKey, session)
	if err != nil {
		return "", c.safeError(err, session)
	}
	return text, nil
}

func (c *MCPClient) safeError(err error, sensitive ...string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", RedactSensitiveText(err.Error(), append(sensitive, c.apiKey)...))
}

// initialize performs initialize + notifications/initialized and returns the
// negotiated Mcp-Session-Id (may be empty if the server does not use one).
func (c *MCPClient) initialize(ctx context.Context) (session, protocolVersion string, err error) {
	protocolVersion = mcpProtocolVersion
	// A server may allocate a session before its initialize response proves
	// parseable or protocol-compatible. Release that state on every failure.
	defer func() {
		if err != nil && session != "" {
			c.terminateSession(session, protocolVersion)
			err = c.safeError(err, session)
		}
	}()

	var initResp *rpcResponse
	initResp, session, err = c.post(ctx, "", "", rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "citadel-memory", "version": "1"},
		},
	})
	if err != nil {
		return session, protocolVersion, err
	}
	if initResp == nil {
		return session, protocolVersion, fmt.Errorf("empty initialize result")
	}
	if initResp.Error != nil {
		return session, protocolVersion, initResp.Error
	}
	if len(initResp.Result) == 0 {
		return session, protocolVersion, fmt.Errorf("empty initialize result")
	}
	var negotiated struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(initResp.Result, &negotiated); err != nil {
		return session, protocolVersion, fmt.Errorf("decode initialize result: %w", err)
	}
	if negotiated.ProtocolVersion != mcpProtocolVersion {
		return session, protocolVersion, fmt.Errorf("unsupported MCP protocol version %q (want %q)", negotiated.ProtocolVersion, mcpProtocolVersion)
	}
	protocolVersion = negotiated.ProtocolVersion

	// Notifications do not have a response body, but transport failures are
	// still fatal: tools/call before initialized is not protocol-conformant.
	var activeSession string
	_, activeSession, err = c.post(ctx, session, protocolVersion, rpcRequest{
		JSONRPC: "2.0", Method: "notifications/initialized",
	})
	if activeSession != "" {
		session = activeSession
	}
	if err != nil {
		return session, protocolVersion, fmt.Errorf("send initialized notification: %w", err)
	}
	return session, protocolVersion, nil
}

// terminateSession releases server-side streamable-HTTP state after each
// short-lived recall/capture call. Cleanup is best-effort and independently
// bounded so it cannot turn a successful Claude hook into a failure or hang.
func (c *MCPClient) terminateSession(session, protocolVersion string) {
	if session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Mcp-Session-Id", session)
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// post sends one JSON-RPC message and returns the parsed response (nil for
// notifications) plus any Mcp-Session-Id the server assigned.
func (c *MCPClient) post(ctx context.Context, session, protocolVersion string, body rpcRequest) (*rpcResponse, string, error) {
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
	if protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
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

	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted &&
		httpResp.StatusCode != http.StatusNoContent {
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 2048))
		safeSnippet := redactSensitivePayload(bytes.TrimSpace(snippet), c.apiKey, session, newSession)
		return nil, newSession, fmt.Errorf("MCP HTTP %d: %s", httpResp.StatusCode, safeSnippet)
	}

	// A notification yields an empty 200/202/204 body.
	if body.Method == "notifications/initialized" {
		_, _ = io.Copy(io.Discard, httpResp.Body)
		return nil, newSession, nil
	}

	resp, err := parseRPCResponse(httpResp.Header.Get("Content-Type"), io.LimitReader(httpResp.Body, maxMCPResponseBytes+1), body.ID)
	if err != nil {
		return nil, newSession, err
	}
	return resp, newSession, nil
}

// parseRPCResponse handles both a plain JSON body and an SSE (text/event-stream)
// body, returning the first JSON-RPC message found.
func parseRPCResponse(contentType string, body io.Reader, expectedID int) (*rpcResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return parseSSE(body, expectedID)
	}
	// Some servers omit/misreport the content type; sniff the first byte.
	buf := bufio.NewReader(body)
	first, err := buf.Peek(1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(first) > 0 && (first[0] == 'e' || first[0] == 'd' || first[0] == ':') {
		return parseSSE(buf, expectedID)
	}
	var resp rpcResponse
	decoder := json.NewDecoder(buf)
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode MCP response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode MCP response: multiple JSON values")
		}
		return nil, fmt.Errorf("decode MCP response trailing data: %w", err)
	}
	if err := validateRPCResponse(&resp, expectedID); err != nil {
		return nil, err
	}
	return &resp, nil
}

func validateRPCResponse(resp *rpcResponse, expectedID int) error {
	if resp.JSONRPC != "2.0" {
		return fmt.Errorf("invalid JSON-RPC version %q", resp.JSONRPC)
	}
	id := bytes.TrimSpace(resp.ID)
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return fmt.Errorf("JSON-RPC response omitted id")
	}
	var got int
	if err := json.Unmarshal(id, &got); err != nil || got != expectedID {
		return fmt.Errorf("JSON-RPC response id %s does not match request id %d", id, expectedID)
	}
	hasResult := resp.Result != nil
	hasError := resp.Error != nil
	if hasResult == hasError {
		return fmt.Errorf("JSON-RPC response must contain exactly one of result or error")
	}
	return nil
}

// parseSSE reads Server-Sent Events and returns the JSON parsed from the first
// complete event whose data payload is a JSON-RPC response. Multiple data lines
// are joined as required by the SSE specification.
func parseSSE(body io.Reader, expectedID int) (*rpcResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lastErr error
	var dataLines []string
	flush := func() *rpcResponse {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var resp rpcResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			return nil
		}
		if resp.Result == nil && resp.Error == nil {
			return nil // notification/server request/unrelated event
		}
		if err := validateRPCResponse(&resp, expectedID); err != nil {
			lastErr = err
			return nil
		}
		return &resp
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if resp := flush(); resp != nil {
				return resp, nil
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		if strings.HasPrefix(data, " ") {
			data = strings.TrimPrefix(data, " ")
		}
		dataLines = append(dataLines, data)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if resp := flush(); resp != nil {
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no matching JSON-RPC response in SSE stream")
}

// decodeToolResult extracts concatenated text from a tools/call result.
func decodeToolResult(raw json.RawMessage, sensitive ...string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", fmt.Errorf("MCP tools/call response omitted result")
	}
	var res toolCallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decode MCP tool result: %w", err)
	}
	hasStructuredContent := len(bytes.TrimSpace(res.StructuredContent)) > 0 &&
		!bytes.Equal(bytes.TrimSpace(res.StructuredContent), []byte("null"))
	if res.Content == nil && !hasStructuredContent && !res.IsError {
		return "", fmt.Errorf("MCP tools/call response contained no result content")
	}
	if res.IsError {
		msg := strings.TrimSpace(toolResultText(res, sensitive...))
		if msg == "" {
			msg = "unspecified tool failure"
		}
		return "", fmt.Errorf("MCP tool reported an error: %s", msg)
	}
	return toolResultText(res, sensitive...), nil
}

func toolResultText(res toolCallResult, sensitive ...string) string {
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
		return redactSensitivePayload(res.StructuredContent, sensitive...)
	}
	return RedactSensitiveText(b.String(), sensitive...)
}
