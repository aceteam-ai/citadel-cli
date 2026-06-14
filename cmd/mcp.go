// cmd/mcp.go
/*
Copyright © 2025 AceTeam <dev@aceteam.ai>
*/
package cmd

import (
	"bufio"
	"bytes"
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
		fmt.Fprintln(os.Stderr, "Error: No API key configured.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Provide an API key using one of:")
		fmt.Fprintln(os.Stderr, "  1. citadel mcp --api-key <key>")
		fmt.Fprintln(os.Stderr, "  2. ACETEAM_API_KEY=<key> citadel mcp")
		fmt.Fprintln(os.Stderr, "  3. citadel init (saves token to config)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Generate an API key at: https://aceteam.ai/settings/api-keys")
		return fmt.Errorf("no API key configured")
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
	}

	Debug("MCP bridge starting: server=%s, url=%s", mcpServer, apiURL)

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
			os.Stdout.Write(resp)
			os.Stdout.Write([]byte("\n"))
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
func (b *mcpBridge) handleInitialize(req *jsonRPCRequest) {
	resp, err := b.forwardToBackend(req)
	if err != nil {
		Debug("MCP: initialize backend error: %v, using local fallback", err)
		// Fallback: return a local initialize response
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
		b.writeResult(req.ID, resultBytes)
		return
	}

	// Write the backend's response directly.
	os.Stdout.Write(resp)
	os.Stdout.Write([]byte("\n"))
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
	os.Stdout.Write(data)
	os.Stdout.Write([]byte("\n"))
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
	os.Stdout.Write(data)
	os.Stdout.Write([]byte("\n"))
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
