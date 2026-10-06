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
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/memory"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	mcpAPIKey       string
	mcpAPIURL       string
	mcpServer       string
	mcpEndpointURL  string
	mcpMemoryConfig bool
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
  3. the node's device config saved by 'citadel init' (device_api_token)

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
	apiKey      string
	apiURL      string // e.g., "https://aceteam.ai"
	mcpServer   string // e.g., "aceteam"
	endpointURL string // optional exact endpoint, otherwise derived from apiURL/server
	sessionID   string // Mcp-Session-Id from the backend
	httpClient  *http.Client

	// localTools are node-local tools served WITHOUT a backend round-trip
	// (aceteam #8249 v1: module control, local inference, workspace files --
	// see cmd/mcp_local.go). Populated once at startup by runMCP.
	localTools []localMCPTool

	// remoteToolAllowlist restricts a credential-specific bridge to the exact
	// backend tools it is allowed to expose and invoke. A nil map means the
	// normal general-purpose bridge; a non-nil map is enforced locally before
	// any authenticated request reaches the backend.
	remoteToolAllowlist map[string]struct{}

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

	// nodeID is this node's own fabric node id (citadel-cli#977,
	// aceteam#8657/#8767), resolved ONCE at startup (runMCP) via
	// resolveNodeIDForMCPHeader -- never re-resolved per request, mirroring
	// stdout above. forwardToBackend attaches it as the X-Citadel-Node-Id
	// request header so the backend's resolve_caller_host_node
	// (python-backend utils/shell_dispatch.py) can recognize a request as
	// originating ON this node instead of routing it through the remote
	// node:exec/passcode gate. Empty on essentially every real node today
	// (see resolveNodeIDForMCPHeader's doc comment) -- forwardToBackend must
	// OMIT the header entirely when this is "", never send it empty.
	nodeID string
}

const maxMCPBackendResponseBytes = 10 << 20

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
	var memoryCfg *memory.Config
	if mcpMemoryConfig {
		var err error
		memoryCfg, err = memory.Load(platform.ConfigDir())
		if err != nil {
			return fmt.Errorf("load memory credential: %w", err)
		}
		if memoryCfg == nil || memoryCfg.APIKey == "" {
			return fmt.Errorf("memory credential is not installed; run 'citadel memory install'")
		}
		if err := memoryCfg.ValidateCredential(); err != nil {
			return fmt.Errorf("refusing unsafe memory credential: %w", err)
		}
		apiKey = memoryCfg.APIKey
	}
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
	if apiURL == "" && memoryCfg != nil {
		apiURL = memoryCfg.APIBaseURL
	}
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
	localTools := newLocalMCPTools(realLocalMCPDeps())
	var remoteToolAllowlist map[string]struct{}
	if memoryCfg != nil {
		// The memory MCP registration is deliberately remote-only. Do not make
		// node-local execution/file tools appear under a credential whose
		// advertised authority is memory:read/write.
		localTools = nil
		remoteToolAllowlist = map[string]struct{}{
			"memory_search": {},
			"memory_write":  {},
		}
	}

	bridge := &mcpBridge{
		apiKey:      apiKey,
		apiURL:      apiURL,
		mcpServer:   mcpServer,
		endpointURL: selectMCPEndpoint(mcpEndpointURL, memoryCfg),
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		localTools:          localTools,
		remoteToolAllowlist: remoteToolAllowlist,
		// Captured HERE, before bridge.run() ever dispatches a tool call, so
		// this is always the real transport -- never a captureStdout pipe.
		// See the stdout field's doc comment.
		stdout: os.Stdout,
		// Resolved HERE, once, rather than per-request -- see the nodeID
		// field's doc comment.
		nodeID: resolveNodeIDForMCPHeader(),
	}

	Debug("MCP bridge starting: server=%s, url=%s, local_tools=%d",
		memory.RedactSensitiveText(mcpServer, apiKey),
		memory.RedactSensitiveText(apiURL, apiKey),
		len(bridge.localTools))

	return bridge.run()
}

func selectMCPEndpoint(explicit string, memoryCfg *memory.Config) string {
	if memoryCfg != nil {
		// --memory-config is a credential-bound mode: never let a generic CLI
		// override redirect its bearer away from the validated saved endpoint.
		return memoryCfg.EffectiveMCPURL()
	}
	return explicit
}

// run starts the stdio JSON-RPC loop.
func (b *mcpBridge) run() error {
	defer b.closeBackendSession()
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

		Debug("MCP: received method=%s id=%s",
			memory.RedactSensitiveText(req.Method, b.apiKey, b.sessionID),
			memory.RedactSensitiveText(string(req.ID), b.apiKey, b.sessionID))

		// A JSON-RPC notification omits id entirely. Null and compound IDs are
		// refused: accepting them would make response correlation ambiguous and
		// would permit objects/arrays to cross the credential-scoped bridge.
		isNotification, err := classifyJSONRPCRequestID(req.ID)
		if err != nil {
			b.writeError(nil, -32600, "Invalid Request")
			continue
		}
		if req.JSONRPC != "2.0" {
			if !isNotification {
				b.writeError(req.ID, -32600, "Invalid Request")
			}
			continue
		}
		if b.remoteToolAllowlist != nil {
			if !b.memoryMethodAllowed(&req) {
				if !isNotification {
					b.writeError(req.ID, -32601, "Method or tool not available")
				}
				continue
			}
		}
		if isNotification {
			// MCP request methods require correlated results and are not executed
			// in notification form. Protocol notifications are forwarded for
			// backend session state, but their response (if any) is discarded.
			if strings.HasPrefix(req.Method, "notifications/") && b.apiKey != "" {
				_, _ = b.forwardToBackend(&req)
			}
			continue
		}

		switch req.Method {
		case "initialize":
			b.handleInitialize(&req)
		case "ping":
			b.writeResult(req.ID, json.RawMessage(`{}`))
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
			// Forward all other requests to the backend.
			resp, err := b.forwardToBackend(&req)
			if err != nil {
				safeErr := memory.RedactSensitiveText(err.Error(), b.apiKey, b.sessionID)
				Debug("MCP: backend error for %s: %s", req.Method, safeErr)
				b.writeError(req.ID, -32603, "Backend error: "+safeErr)
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

// classifyJSONRPCRequestID returns true only when the id member is absent.
// This bridge accepts the interoperable JSON-RPC ID forms used by MCP clients:
// strings and numbers. Explicit null is rejected rather than conflated with an
// absent notification ID.
func classifyJSONRPCRequestID(raw json.RawMessage) (bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true, nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return false, fmt.Errorf("null JSON-RPC id")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false, fmt.Errorf("decode JSON-RPC id: %w", err)
	}
	switch value.(type) {
	case string, json.Number:
		return false, nil
	default:
		return false, fmt.Errorf("JSON-RPC id must be a string or number")
	}
}

func (b *mcpBridge) memoryMethodAllowed(req *jsonRPCRequest) bool {
	switch req.Method {
	case "initialize", "ping", "tools/list", "notifications/initialized":
		return true
	case "tools/call":
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			return false
		}
		_, ok := b.remoteToolAllowlist[params.Name]
		return ok
	default:
		return false
	}
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
		Debug("MCP: initialize backend error: %s, using local fallback",
			memory.RedactSensitiveText(err.Error(), b.apiKey, b.sessionID))
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
		Debug("MCP: tools/list backend error: %s; serving local tools only",
			memory.RedactSensitiveText(err.Error(), b.apiKey, b.sessionID))
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
	if b.remoteToolAllowlist != nil {
		filtered := make([]json.RawMessage, 0, len(backendTools))
		for _, raw := range backendTools {
			var descriptor struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &descriptor) == nil {
				if _, ok := b.remoteToolAllowlist[descriptor.Name]; ok {
					filtered = append(filtered, raw)
				}
			}
		}
		backendTools = filtered
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

	Debug("MCP: dispatching local tool %q", memory.RedactSensitiveText(tool.Name, b.apiKey, b.sessionID))
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

	url := b.backendEndpointURL()
	// The configured URL may itself contain operator-supplied diagnostics or
	// legacy query material. Do not copy it to stderr; the method is sufficient
	// to correlate a failed bridge request.
	Debug("MCP: POST backend (method=%s)", req.Method)

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	// MCP Streamable HTTP requires the client to accept both JSON and SSE.
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+b.apiKey)

	// x-citadel-node-id (citadel-cli#977): a locality signal for the
	// backend's resolve_caller_host_node, carrying this node's own fabric
	// node id -- see the nodeID field's doc comment. Omitted entirely (not
	// sent empty) when unresolved: an empty header is worse than no header,
	// since a naive backend check for header presence would otherwise read
	// as "caller claims to be node ''" instead of "caller sent no claim".
	if b.nodeID != "" {
		httpReq.Header.Set("X-Citadel-Node-Id", b.nodeID)
	}

	// Include session ID if we have one from a previous initialize.
	if b.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", b.sessionID)
	}

	httpResp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer httpResp.Body.Close()
	responseSession := httpResp.Header.Get("Mcp-Session-Id")

	// 200 = success with body, 202 = accepted (notifications), both are OK.
	if httpResp.StatusCode != http.StatusOK && httpResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 64<<10))
		safeBody := string(b.redactBackendPayload(body, responseSession))
		Debug("MCP: backend returned %d: %s", httpResp.StatusCode, safeBody)
		return nil, fmt.Errorf("backend returned HTTP %d: %s", httpResp.StatusCode, truncate(safeBody, 200))
	}

	// Only a successful protocol response may establish or rotate the session.
	// An error response's headers must not poison the next request.
	if responseSession != "" {
		b.sessionID = responseSession
		Debug("MCP: backend session established")
	}

	// 202 Accepted is valid only for notifications. A correlated JSON-RPC
	// request still requires a response carrying exactly one of result/error;
	// accepting an empty 202 here would leave the stdio client waiting forever.
	if httpResp.StatusCode == http.StatusAccepted {
		isNotification, idErr := classifyJSONRPCRequestID(req.ID)
		if idErr != nil || !isNotification {
			return nil, fmt.Errorf("backend returned HTTP 202 for a JSON-RPC request")
		}
		return nil, nil
	}

	contentType := httpResp.Header.Get("Content-Type")
	Debug("MCP: response Content-Type: %s", memory.RedactSensitiveText(contentType, b.apiKey, b.sessionID))

	if strings.Contains(contentType, "text/event-stream") {
		return b.parseSSEResponseExpected(httpResp.Body, req.ID, b.remoteToolAllowlist != nil)
	}

	// Plain JSON response.
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxMCPBackendResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxMCPBackendResponseBytes {
		return nil, fmt.Errorf("backend response exceeds %d bytes", maxMCPBackendResponseBytes)
	}
	body = b.redactBackendPayload(body)
	if b.remoteToolAllowlist != nil && len(req.ID) > 0 && string(req.ID) != "null" {
		if err := validateBackendRPCResponse(body, req.ID); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func (b *mcpBridge) backendEndpointURL() string {
	if b.endpointURL != "" {
		return b.endpointURL
	}
	return fmt.Sprintf("%s/api/mcp/%s/mcp", b.apiURL, b.mcpServer)
}

// closeBackendSession releases the server-side MCP session when the stdio
// client disconnects. It is deliberately best-effort and tightly bounded;
// process shutdown must not hang or emit protocol noise to stdout.
func (b *mcpBridge) closeBackendSession() {
	if b.sessionID == "" || b.apiKey == "" || b.httpClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.backendEndpointURL(), nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+b.apiKey)
	req.Header.Set("Mcp-Session-Id", b.sessionID)
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	b.sessionID = ""
}

func validateBackendRPCResponse(data, expectedID json.RawMessage) error {
	var resp jsonRPCResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("decode JSON-RPC response: %w", err)
	}
	if resp.JSONRPC != "2.0" {
		return fmt.Errorf("invalid backend JSON-RPC version %q", resp.JSONRPC)
	}
	gotID := bytes.TrimSpace(resp.ID)
	wantID := bytes.TrimSpace(expectedID)
	if len(gotID) == 0 || bytes.Equal(gotID, []byte("null")) {
		return fmt.Errorf("backend JSON-RPC response omitted id")
	}
	if !bytes.Equal(gotID, wantID) {
		return fmt.Errorf("backend JSON-RPC response id %s does not match request id %s", gotID, wantID)
	}
	hasResult := resp.Result != nil
	hasError := resp.Error != nil
	if hasResult == hasError {
		return fmt.Errorf("backend JSON-RPC response must contain exactly one of result or error")
	}
	return nil
}

// parseSSEResponse reads an SSE stream and extracts JSON-RPC messages from
// "data:" lines within "event: message" frames. Returns the last JSON-RPC
// response or error message found (the final result for this request).
func (b *mcpBridge) parseSSEResponse(body io.Reader) ([]byte, error) {
	return b.parseSSEResponseExpected(body, nil, false)
}

func (b *mcpBridge) parseSSEResponseExpected(body io.Reader, expectedID json.RawMessage, strict bool) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)

	var lastResponse []byte
	var lastValidationErr error
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

			data = string(b.redactBackendPayload([]byte(data)))
			Debug("MCP: SSE data: %s", truncate(data, 200))
			candidate := []byte(data)
			if strict {
				if err := validateBackendRPCResponse(candidate, expectedID); err != nil {
					lastValidationErr = err
					continue
				}
				return candidate, nil
			}
			lastResponse = candidate
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
		if lastValidationErr != nil {
			return nil, lastValidationErr
		}
		return nil, fmt.Errorf("no JSON-RPC message found in SSE stream")
	}

	return lastResponse, nil
}

func (b *mcpBridge) redactBackendPayload(data []byte, extra ...string) []byte {
	sensitive := append([]string{b.apiKey, b.sessionID}, extra...)
	if redacted, err := memory.RedactSensitiveJSON(data, sensitive...); err == nil {
		return redacted
	}
	return []byte(memory.RedactSensitiveText(string(data), sensitive...))
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

// getAPIKeyFromConfig reads the device API token from the citadel device
// config, via getDeviceConfigFromFile -- the same converged (machine-
// convergent-dir-first, legacy-ConfigDir-fallback) read every other cmd/
// call site uses, so `citadel mcp` agrees with `citadel init`/`citadel work`
// on where the token lives regardless of invocation context (citadel-cli#845).
func getAPIKeyFromConfig() string {
	if dc := getDeviceConfigFromFile(); dc != nil {
		return dc.DeviceAPIToken
	}
	return ""
}

// getAPIURLFromConfig reads the API base URL from the citadel device config
// (see getAPIKeyFromConfig).
func getAPIURLFromConfig() string {
	if dc := getDeviceConfigFromFile(); dc != nil {
		return dc.APIBaseURL
	}
	return ""
}

// resolveNodeIDForMCPHeader resolves this node's own fabric/platform node id
// for the X-Citadel-Node-Id request header (citadel-cli#977,
// aceteam#8657/#8767 slice 1): one of the two locality signals the backend's
// resolve_caller_host_node (python-backend utils/shell_dispatch.py) uses to
// recognize a request as originating ON this node, so an agent running here
// gets redirected to its own local shell instead of the remote
// node:exec/passcode gate.
//
// Reuses cmd/whoami.go's exact platform-node-id preference order via the
// shared resolvePlatformNodeID helper (DeviceConfig.FabricNodeID, aceteam
// #8139, wins when present; falls back to the legacy SSHSyncConfig.NodeID
// slot) -- deliberately NOT gatherIdentity itself, which additionally
// performs a live mesh reconnect probe and writes identity.json. Neither
// belongs on citadel mcp's startup path: this bridge needs a fast, local,
// side-effect-free read, not a network round-trip or a cache write.
//
// Returns "" when unresolved -- which is the common case on every real node
// today, since no backend process yet echoes FabricNodeID back to a node
// (see cmd/whoami.go's package doc comment). Callers must omit the header
// entirely on "", never send it empty.
func resolveNodeIDForMCPHeader() string {
	var fabricNodeID string
	if dc := getDeviceConfigFromFile(); dc != nil {
		fabricNodeID = dc.FabricNodeID
	}

	nodeConfigDir := network.GetNodeConfigDir()
	var sshSyncNodeID string
	if sshConfig, err := nexus.LoadSSHSyncConfig(nodeConfigDir); err == nil && sshConfig != nil {
		sshSyncNodeID = sshConfig.NodeID
	}

	return resolvePlatformNodeID(fabricNodeID, sshSyncNodeID)
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
	mcpCmd.Flags().StringVar(&mcpEndpointURL, "endpoint-url", "", "Exact MCP endpoint URL (overrides --api-url/--server except in --memory-config mode)")
	mcpCmd.Flags().BoolVar(&mcpMemoryConfig, "memory-config", false, "Read the scoped key and endpoint from memory.yaml")
}
