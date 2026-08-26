// cmd/mcp.go
/*
Copyright © 2025 AceTeam <dev@aceteam.ai>
*/
package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	mcpAPIKey string
	mcpAPIURL string
	mcpServer string
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start a local MCP server for AI tool integration",
	Long: `Starts a Model Context Protocol (MCP) server that exposes AceTeam tools
to Claude Code, Cursor, and other AI development tools.

The MCP server reads JSON-RPC messages from stdin and writes responses to
stdout, bridging your local AI tools to the AceTeam platform.

Authentication uses an AceTeam API key. Generate one at:
  https://aceteam.ai/settings/api-keys

The API key is read from (in priority order):
  1. --api-key flag
  2. ACETEAM_API_KEY environment variable
  3. ~/.citadel-cli/config.yaml (device_api_token from 'citadel init')

Usage with Claude Code:
  claude mcp add aceteam -- citadel mcp

Usage with Cursor (add to .cursor/mcp.json):
  {"mcpServers": {"aceteam": {"command": "citadel", "args": ["mcp"]}}}

Usage with environment variable:
  ACETEAM_API_KEY=act_xxx claude mcp add aceteam -- citadel mcp`,
	RunE: runMCP,
}

// jsonRPCRequest represents an incoming JSON-RPC 2.0 request or notification.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse represents an outgoing JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCError represents a JSON-RPC 2.0 error object.
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpBridge holds the state for the stdio-to-HTTP MCP bridge.
type mcpBridge struct {
	apiKey     string
	apiURL     string // e.g., "https://aceteam.ai"
	mcpServer  string // e.g., "aceteam"
	sessionID  string // Mcp-Session-Id from the backend
	httpClient *http.Client

	// localTools are node-local tools served WITHOUT a backend round-trip
	// (aceteam #8249 v1: module control, local inference, workspace files --
	// see cmd/mcp_local.go). Populated once at startup by runMCP.
	localTools []localMCPTool

	// stdout is the JSON-RPC transport writer, captured ONCE at startup
	// (runMCP) before any tool call can run. citadel#858: reading the live
	// os.Stdout variable at write time (the pre-#858 behavior) is unsafe once
	// callLocalToolWithTimeout can abandon a local tool call -- the abandoned
	// goroutine can still be inside captureStdout, holding the process-wide
	// os.Stdout variable redirected to ITS pipe, for as long as the
	// underlying `docker compose` call keeps running. A response written via
	// the live os.Stdout variable during that window would silently land in
	// the orphan's pipe instead of reaching the client. Writing through this
	// saved reference instead sidesteps that entirely: captureStdout swaps
	// the *variable*, never the object this field already points at. Falls
	// back to the live os.Stdout via bridgeStdout() when unset (every
	// existing test constructs a bare &mcpBridge{} and relies on that).
	stdout io.Writer
}

// bridgeStdout returns the JSON-RPC transport writer -- see the stdout
// field's doc comment for why this must NOT simply read os.Stdout at write
// time.
func (b *mcpBridge) bridgeStdout() io.Writer {
	if b.stdout != nil {
		return b.stdout
	}
	return os.Stdout
}

func runMCP(cmd *cobra.Command, args []string) error {
	// Debug output must go to stderr, not stdout — stdout is the JSON-RPC transport.
	debugToStderr = true

	// Resolve API key
	apiKey := mcpAPIKey
	if apiKey == "" {
		apiKey = os.Getenv("ACETEAM_API_KEY")
	}
	if apiKey == "" {
		apiKey = getAPIKeyFromConfig()
	}
	if apiKey == "" {
		// No API key does NOT mean "refuse to start" -- the local tools
		// (module control, local inference, workspace files; aceteam #8249)
		// are LOCAL authority: they run on this node for the node owner and
		// never call the AceTeam backend, so they work with zero central
		// credentials. Only the remote/fabric tool set (proxied to the
		// backend below) is unavailable in this mode.
		fmt.Fprintln(os.Stderr, "Note: No AceTeam API key configured -- remote AceTeam tools unavailable.")
		fmt.Fprintln(os.Stderr, "Local node tools (module control, local inference, workspace files) are still served.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "To also enable remote AceTeam tools, provide an API key using one of:")
		fmt.Fprintln(os.Stderr, "  1. citadel mcp --api-key <key>")
		fmt.Fprintln(os.Stderr, "  2. ACETEAM_API_KEY=<key> citadel mcp")
		fmt.Fprintln(os.Stderr, "  3. citadel init (saves token to config)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Generate an API key at: https://aceteam.ai/settings/api-keys")
	}

	// Resolve API URL
	apiURL := mcpAPIURL
	if apiURL == "" {
		apiURL = os.Getenv("ACETEAM_URL")
	}
	if apiURL == "" {
		apiURL = getAPIURLFromConfig()
	}
	if apiURL == "" {
		apiURL = "https://aceteam.ai"
	}
	// Strip trailing slash
	apiURL = strings.TrimRight(apiURL, "/")

	bridge := &mcpBridge{
		apiKey:    apiKey,
		apiURL:    apiURL,
		mcpServer: mcpServer,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		localTools: newLocalMCPTools(realLocalMCPDeps()),
		// Captured HERE, before bridge.run() ever dispatches a tool call, so
		// this is always the real transport -- never a captureStdout pipe.
		// See the stdout field's doc comment.
		stdout: os.Stdout,
	}

	Debug("MCP bridge starting: server=%s, url=%s, local_tools=%d", mcpServer, apiURL, len(bridge.localTools))

	return bridge.run()
}

// run starts the stdio JSON-RPC loop.
func (b *mcpBridge) run() error {
	scanner := bufio.NewScanner(os.Stdin)
	// Increase buffer size to handle large tool lists (default 64KB is too small).
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			Debug("MCP: failed to parse JSON-RPC request: %v", err)
			// If we can't parse it, and it might have an ID, send a parse error
			b.writeError(nil, -32700, "Parse error")
			continue
		}

		Debug("MCP: received method=%s id=%s", req.Method, string(req.ID))

		// Notifications have no ID and must not receive a response.
		isNotification := len(req.ID) == 0 || string(req.ID) == "null"

		switch req.Method {
		case "initialize":
			b.handleInitialize(&req)
		case "ping":
			if !isNotification {
				b.writeResult(req.ID, json.RawMessage(`{}`))
			}
		case "tools/list":
			// Always includes the local tool set (aceteam #8249), merged with
			// the backend's remote tool set when a backend is reachable.
			b.handleToolsList(&req)
		case "tools/call":
			// A tools/call for one of OUR local tool names is dispatched here,
			// entirely without a backend round-trip. Anything else falls
			// through to the same backend-forwarding path every other method
			// uses (below).
			if b.tryLocalToolsCall(&req) {
				continue
			}
			fallthrough
		default:
			if isNotification {
				// Forward notifications to the backend but don't write a response.
				_, _ = b.forwardToBackend(&req)
				continue
			}
			// Forward all other requests to the backend.
			resp, err := b.forwardToBackend(&req)
			if err != nil {
				Debug("MCP: backend error for %s: %v", req.Method, err)
				b.writeError(req.ID, -32603, fmt.Sprintf("Backend error: %v", err))
				continue
			}
			if resp == nil {
				// No body (e.g. 202 Accepted) -- should not happen for requests.
				Debug("MCP: empty response for %s", req.Method)
				continue
			}
			// Write the raw response directly to stdout.
			b.bridgeStdout().Write(resp)
			b.bridgeStdout().Write([]byte("\n"))
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stdin read error: %w", err)
	}
	return nil
}

// handleInitialize handles the MCP initialize request.
// We forward to the backend so it creates a session, but we also ensure
// the response includes the required fields.
//
// With no API key configured there is no backend session to create -- local
// tools (aceteam #8249) don't need one -- so this short-circuits straight to
// the local response instead of forwarding first and waiting on a call that
// will only fail.
func (b *mcpBridge) handleInitialize(req *jsonRPCRequest) {
	if b.apiKey == "" {
		b.writeLocalInitializeResult(req.ID)
		return
	}

	resp, err := b.forwardToBackend(req)
	if err != nil {
		Debug("MCP: initialize backend error: %v, using local fallback", err)
		b.writeLocalInitializeResult(req.ID)
		return
	}

	// Write the backend's response directly.
	b.bridgeStdout().Write(resp)
	b.bridgeStdout().Write([]byte("\n"))
}

// writeLocalInitializeResult writes a self-contained initialize response with
// no backend session, used both when no API key is configured and as the
// fallback when the backend is unreachable.
func (b *mcpBridge) writeLocalInitializeResult(id json.RawMessage) {
	result := map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "aceteam",
			"version": Version,
		},
	}
	resultBytes, _ := json.Marshal(result)
	b.writeResult(id, resultBytes)
}

// handleToolsList responds to tools/list with the local tool set (aceteam
// #8249) merged into the backend's remote tool set. With no API key, or on a
// backend error, it serves the local tools only rather than failing the
// whole listing -- an agent should still see (and be able to use) the node's
// own local tools even when remote AceTeam tools are unavailable.
func (b *mcpBridge) handleToolsList(req *jsonRPCRequest) {
	localRaw := make([]json.RawMessage, 0, len(b.localTools))
	for _, t := range b.localTools {
		desc, err := json.Marshal(map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
		if err != nil {
			Debug("MCP: failed to marshal local tool %q: %v", t.Name, err)
			continue
		}
		localRaw = append(localRaw, desc)
	}

	if b.apiKey == "" {
		b.writeToolsListResult(req.ID, localRaw)
		return
	}

	resp, err := b.forwardToBackend(req)
	if err != nil {
		Debug("MCP: tools/list backend error: %v; serving local tools only", err)
		b.writeToolsListResult(req.ID, localRaw)
		return
	}

	var parsed struct {
		Result map[string]json.RawMessage `json:"result"`
		Error  *jsonRPCError              `json:"error"`
	}
	if uerr := json.Unmarshal(resp, &parsed); uerr != nil || parsed.Result == nil || parsed.Error != nil {
		Debug("MCP: tools/list backend response unusable (parse=%v, error=%v); serving local tools only", uerr, parsed.Error)
		b.writeToolsListResult(req.ID, localRaw)
		return
	}

	var backendTools []json.RawMessage
	if raw, ok := parsed.Result["tools"]; ok {
		_ = json.Unmarshal(raw, &backendTools)
	}
	merged := append(backendTools, localRaw...)
	mergedTools, err := json.Marshal(merged)
	if err != nil {
		Debug("MCP: failed to marshal merged tools/list: %v", err)
		b.writeToolsListResult(req.ID, localRaw)
		return
	}
	parsed.Result["tools"] = mergedTools

	resultBytes, err := json.Marshal(parsed.Result)
	if err != nil {
		Debug("MCP: failed to marshal merged tools/list result: %v", err)
		b.writeToolsListResult(req.ID, localRaw)
		return
	}
	b.writeResult(req.ID, resultBytes)
}

// writeToolsListResult writes a tools/list result carrying exactly the given
// tools (used for the local-only fallback paths).
func (b *mcpBridge) writeToolsListResult(id json.RawMessage, tools []json.RawMessage) {
	if tools == nil {
		tools = []json.RawMessage{}
	}
	result, err := json.Marshal(map[string]any{"tools": tools})
	if err != nil {
		Debug("MCP: failed to marshal tools/list result: %v", err)
		b.writeError(id, -32603, "failed to build tools/list result")
		return
	}
	b.writeResult(id, result)
}

// tryLocalToolsCall dispatches a tools/call request to a local tool
// (aceteam #8249) when its "name" matches one, entirely without a backend
// round-trip. Returns false (having written nothing) when the requested tool
// is not one of ours, so the caller falls through to normal backend
// forwarding.
func (b *mcpBridge) tryLocalToolsCall(req *jsonRPCRequest) bool {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		// Malformed params -- let the backend's own validation produce the
		// error rather than guessing here.
		return false
	}

	tool, ok := findLocalTool(b.localTools, params.Name)
	if !ok {
		return false
	}

	Debug("MCP: dispatching local tool %q", tool.Name)
	ctx, cancel := context.WithTimeout(context.Background(), localToolCallTimeout)
	defer cancel()
	text, err := callLocalToolWithTimeout(ctx, tool, params.Arguments)

	var result map[string]any
	if err != nil {
		Debug("MCP: local tool %q failed: %v", tool.Name, err)
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	} else {
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": false,
		}
	}

	resultBytes, merr := json.Marshal(result)
	if merr != nil {
		b.writeError(req.ID, -32603, fmt.Sprintf("failed to marshal local tool result: %v", merr))
		return true
	}
	b.writeResult(req.ID, resultBytes)
	return true
}

// callLocalToolWithTimeout makes localToolCallTimeout actually bound a local
// tool call, rather than merely decorating one. Handing tool.Call a context
// with a deadline is not enough on its own: local_module_stop/start/restart's
// underlying primitive (runModuleControl -> a synchronous `docker compose
// up|down` exec.Cmd.Run(), see localToolCallTimeout's doc comment) has no
// cancellation hook, so a caller that just awaited tool.Call(ctx, ...)
// directly would still block on it regardless of ctx.Done() -- exactly the
// bug this closes: a wedged module action used to stall the entire
// single-threaded JSON-RPC loop (cmd/mcp_local.go's mcpBridge.run), not just
// its own request.
//
// This mirrors the worker consume-loop watchdog's exact tradeoff
// (internal/worker/deadline.go's executeWithDeadline): run the call in its
// own goroutine and race it against ctx.Done(). On timeout this function
// returns immediately with a timeout error and the JSON-RPC loop moves on to
// the next request; the goroutine is NOT killed and keeps running the
// (possibly still-synchronous, still-stdout-capturing) tool call to
// completion in the background. That is an accepted, documented leak, not an
// oversight -- see executeWithDeadline's comment for why a handler that
// ignores cancellation makes this the only option short of threading real
// cancellation into every call chain (out of scope here; see
// localToolCallTimeout's own comment on why that's a separate, harder
// follow-up for the module-control primitives specifically).
//
// Two consequences of the abandoned goroutine, both closed elsewhere -- read
// this alongside those, don't assume this function alone makes timeout
// handling safe:
//   - The abandoned goroutine still holds captureStdout's os.Stdout
//     redirection open until it finishes, so a SECOND local tool call
//     dispatched while it's still running would otherwise race the same
//     os.Stdout variable (the MCP stdio loop is otherwise single-threaded and
//     synchronous -- see captureStdout's doc comment -- so this is the one
//     place that invariant can be violated). captureStdout's
//     stdoutCaptureInFlight guard closes this: it refuses to nest and returns
//     a clear error instead of corrupting os.Stdout or blocking.
//   - This function's own timeout error still has to reach the client, and it
//     must NOT be written via a possibly-still-redirected os.Stdout. The
//     caller (tryLocalToolsCall, via b.writeResult/writeError) writes through
//     mcpBridge.stdout -- a reference captured once at startup, before any
//     tool call could ever redirect the live os.Stdout variable -- so the
//     timeout response reaches the real transport even while an orphaned
//     goroutine elsewhere still has os.Stdout pointed at its own pipe. See
//     the stdout field's doc comment on mcpBridge.
func callLocalToolWithTimeout(ctx context.Context, tool localMCPTool, args json.RawMessage) (string, error) {
	type callResult struct {
		text string
		err  error
	}
	// Buffered (size 1) so an abandoned goroutine that finishes after we've
	// already given up on it can still send its result and exit, rather than
	// leaking blocked on the channel send forever.
	done := make(chan callResult, 1)
	go func() {
		text, err := tool.Call(ctx, args)
		done <- callResult{text: text, err: err}
	}()

	select {
	case r := <-done:
		return r.text, r.err
	case <-ctx.Done():
		Debug("MCP: local tool %q timed out after %s (orphaned goroutine will finish in the background)", tool.Name, localToolCallTimeout)
		return "", fmt.Errorf("local tool %s timed out after %s", tool.Name, localToolCallTimeout)
	}
}

// forwardToBackend sends a JSON-RPC request to the AceTeam MCP backend
// via HTTP POST and returns the JSON-RPC response bytes.
//
// The MCP Streamable HTTP transport may return either:
//   - application/json: the response is a raw JSON-RPC message
//   - text/event-stream: the response is an SSE stream containing one or more
//     "event: message\ndata: {json}\n\n" frames. We parse the data lines and
//     return the last JSON-RPC response/error message found.
func (b *mcpBridge) forwardToBackend(req *jsonRPCRequest) ([]byte, error) {
	// Re-serialize the request to forward.
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/api/mcp/%s/mcp", b.apiURL, b.mcpServer)
	Debug("MCP: POST %s (method=%s)", url, req.Method)

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// MCP Streamable HTTP requires the client to accept both JSON and SSE.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)

	// Include session ID if we have one from a previous initialize.
	if b.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", b.sessionID)
	}

	httpResp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer httpResp.Body.Close()

	// Capture session ID from response.
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		b.sessionID = sid
		Debug("MCP: session ID: %s", sid)
	}

	// 200 = success with body, 202 = accepted (notifications), both are OK.
	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(httpResp.Body)
		Debug("MCP: backend returned %d: %s", httpResp.StatusCode, string(body))
		return nil, fmt.Errorf("backend returned HTTP %d: %s", httpResp.StatusCode, truncate(string(body), 200))
	}

	// 202 Accepted has no body (used for notifications).
	if httpResp.StatusCode == http.StatusAccepted {
		return nil, nil
	}

	contentType := httpResp.Header.Get("Content-Type")
	Debug("MCP: response Content-Type: %s", contentType)

	if strings.Contains(contentType, "text/event-stream") {
		return b.parseSSEResponse(httpResp.Body)
	}

	// Plain JSON response.
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}

// parseSSEResponse reads an SSE stream and extracts JSON-RPC messages from
// "data:" lines within "event: message" frames. Returns the last JSON-RPC
// response or error message found (the final result for this request).
func (b *mcpBridge) parseSSEResponse(body io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	var lastResponse []byte
	inMessageEvent := false

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event:") {
			eventType := strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			inMessageEvent = (eventType == "message")
			continue
		}

		if strings.HasPrefix(line, "data:") && inMessageEvent {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			if data == "" {
				continue
			}

			Debug("MCP: SSE data: %s", truncate(data, 200))
			lastResponse = []byte(data)
		}

		// Empty line marks end of an SSE event frame.
		if line == "" {
			inMessageEvent = false
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE read error: %w", err)
	}

	if lastResponse == nil {
		return nil, fmt.Errorf("no JSON-RPC message found in SSE stream")
	}

	return lastResponse, nil
}

// writeResult writes a successful JSON-RPC response to stdout.
func (b *mcpBridge) writeResult(id json.RawMessage, result json.RawMessage) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, err := json.Marshal(resp)
	if err != nil {
		Debug("MCP: failed to marshal response: %v", err)
		return
	}
	b.bridgeStdout().Write(data)
	b.bridgeStdout().Write([]byte("\n"))
}

// writeError writes an error JSON-RPC response to stdout.
func (b *mcpBridge) writeError(id json.RawMessage, code int, message string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		Debug("MCP: failed to marshal error response: %v", err)
		return
	}
	b.bridgeStdout().Write(data)
	b.bridgeStdout().Write([]byte("\n"))
}

// getAPIKeyFromConfig reads the device API token from the citadel config file.
func getAPIKeyFromConfig() string {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return ""
	}

	var config struct {
		DeviceAPIToken string `yaml:"device_api_token"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	return config.DeviceAPIToken
}

// getAPIURLFromConfig reads the API base URL from the citadel config file.
func getAPIURLFromConfig() string {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return ""
	}

	var config struct {
		APIBaseURL string `yaml:"api_base_url"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	return config.APIBaseURL
}

// truncate shortens a string to maxLen, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func init() {
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.Flags().StringVar(&mcpAPIKey, "api-key", "", "AceTeam API key (or set ACETEAM_API_KEY env)")
	mcpCmd.Flags().StringVar(&mcpAPIURL, "api-url", "", "AceTeam API URL (default: https://aceteam.ai)")
	mcpCmd.Flags().StringVar(&mcpServer, "server", "aceteam", "MCP server name to proxy (default: aceteam)")
}
