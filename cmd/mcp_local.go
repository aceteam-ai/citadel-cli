// cmd/mcp_local.go
//
// aceteam #8249 v1: `citadel mcp` gains a set of LOCAL tools -- module
// stop/start/restart, local inference chat, and read-only workspace file
// access -- that run entirely on this node for the node owner, with NO
// round-trip to the central AceTeam platform and no `node:modules` scope.
// Before this, the only "local control" available to an agent/user on the
// node was hand-editing citadel.yaml or shelling out to the CLI directly;
// this closes that gap the way `citadel mcp` already closes it for remote
// fabric management (proxied to the AceTeam backend in cmd/mcp.go).
//
// Deliberately out of scope for v1 (documented, not silently dropped): model
// deploy/evict and `run --exclusive`. Those ride a VRAM-reservation/eviction
// path (aceteam #8248 part 2, citadel #832/#851) that is not merged yet, and
// wiring them here ahead of that primitive would risk the exact
// destructive/worklock hazards #832/#851 exist to prevent.
//
// Every tool defined here is prefixed "local_" -- not a style choice, a
// collision rule: the AceTeam backend's own MCP tool set already has
// `service_start`/`service_stop`/`service_status`, `code_read`/`code_list`,
// and `node_module_set`/`node_modules_list`, all of which operate on a
// REMOTE node chosen by node_id. An LLM confusing "stop the vllm service on
// MY node" with the remote `service_stop(node_id, ...)` tool would silently
// act on the wrong node. The "local_" prefix makes the target unambiguous
// from the tool name alone, and MUST stay unique against every backend tool
// name -- see TestLocalToolNamesDoNotCollideWithKnownBackendTools.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/mesh"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// localMCPTool is one locally-served MCP tool. Call receives the raw
// "arguments" object from the tools/call request and returns the tool's text
// result (wrapped into the standard {content:[{type:text,...}]} shape by the
// caller) or an error (wrapped into isError:true).
type localMCPTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(ctx context.Context, args json.RawMessage) (string, error)
}

// localMCPDeps are the real collaborators newLocalMCPTools wires its tools to.
// A struct (not free functions) so tests can substitute every side-effecting
// dependency -- module control, engine discovery, engine HTTP dialing,
// filesystem -- without touching a live manifest, container runtime, or disk
// outside a test's own tempdir.
type localMCPDeps struct {
	// moduleControl performs the actual scoped stop/start/restart. Production
	// wires runModuleControlCaptured, which drives the SAME primitive
	// `citadel module stop|start|restart` uses (runModuleControl, #846) through
	// captureStdout so its (and docker compose's) stdout writes never reach the
	// JSON-RPC transport. Tests inject a stub so exercising the tool wiring
	// never reads the live manifest or touches a real container.
	moduleControl moduleControlFn

	// chatLister returns this node's currently serving local engines and their
	// models. Production wires newLocalChatLister() (cmd/gateway_chat.go),
	// shared with the gateway's own chat route so both agree on what "locally
	// served" means.
	chatLister gateway.ChatModelLister

	// chatClient dials the resolved engine's OpenAI-compatible endpoint.
	// Production wires a plain loopback dialer (no mesh, no central Redis);
	// tests inject a dialer that redirects to an httptest server, mirroring
	// internal/mesh's own test pattern.
	chatClient *mesh.Client

	// workspaceDir is the sandbox root for the file tools. Production wires
	// resolveWorkspaceDir() (cmd/work.go) -- the SAME sandbox the FILE_READ/
	// FILE_LIST job handlers use, so a path this tool can read is exactly a
	// path a dispatched FILE_READ job could read, not a wider surface.
	workspaceDir string

	// reservations backs local_model_deploy/local_run_exclusive/
	// local_model_stop (aceteam#8248/#8249 v2, docs/design-model-exclusivity.md).
	// Production wires realLocalReservationOps(), which constructs a real
	// internal/jobs.ServiceHandler per call (resolving the manifest config
	// dir fresh each time, mirroring moduleControl's own pattern). Tests
	// inject stubs so exercising the tool wiring never reads a live
	// manifest, container runtime, or GPU.
	reservations localReservationOps
}

// moduleControlFn is the shape of the actual stop/start/restart action. See
// localMCPDeps.moduleControl.
type moduleControlFn func(name string, action moduleAction) (string, error)

// realLocalMCPDeps builds the production dependency set. Called once per
// `citadel mcp` process (cmd/mcp.go:runMCP), not per request.
func realLocalMCPDeps() localMCPDeps {
	return localMCPDeps{
		moduleControl: runModuleControlCaptured,
		chatLister:    newLocalChatLister(),
		chatClient:    mesh.NewClient((&net.Dialer{}).DialContext),
		workspaceDir:  resolveWorkspaceDir(),
		reservations:  realLocalReservationOps(),
	}
}

// newLocalMCPTools builds the full local tool set from the given
// dependencies. Kept separate from realLocalMCPDeps so tests can build the
// tool set from a stubbed localMCPDeps without touching runMCP at all.
func newLocalMCPTools(deps localMCPDeps) []localMCPTool {
	tools := newModuleControlTools(deps.moduleControl)
	tools = append(tools, newLocalInferenceTools(deps)...)
	tools = append(tools, newLocalFileTools(deps.workspaceDir)...)
	tools = append(tools, newModelExclusivityTools(deps)...)
	return tools
}

// findLocalTool looks up a tool by exact name.
func findLocalTool(tools []localMCPTool, name string) (localMCPTool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return localMCPTool{}, false
}

// localToolCallTimeout bounds how long tryLocalToolsCall (cmd/mcp.go) waits
// on ANY local tool call before giving up on it and letting the JSON-RPC loop
// move on -- see callLocalToolWithTimeout, which is what makes this real
// rather than merely decorative (citadel #858; a prior version of this
// comment documented the deadline as inert for module control because
// tool.Call used to be awaited directly).
//
// What this bounds and what it doesn't, per tool:
//   - local_chat: deps.chatClient.ChatCompletion also honors ctx directly, so
//     on timeout the underlying HTTP request is ACTUALLY cancelled, not just
//     abandoned.
//   - local_module_stop/start/restart: moduleControlFn's signature has no ctx
//     parameter, because the primitive it drives (runModuleControl ->
//     liveModuleOps.Start/Stop -> `docker compose up|down` via a synchronous
//     exec.Cmd.Run()) has no cancellation hook to wire one into. On timeout
//     the CALLER still gets its response back promptly (callLocalToolWithTimeout
//     races the call against ctx.Done() in a separate goroutine), but the
//     underlying docker compose invocation is NOT killed -- it keeps running
//     in the background, still holding captureStdout's os.Stdout redirection,
//     until the subprocess itself exits (see captureStdout's doc comment for
//     why that pipe swap can only be undone by the subprocess exiting). This
//     is the SAME accepted-leak tradeoff as the worker consume-loop watchdog
//     (internal/worker/deadline.go's executeWithDeadline) makes for legacy
//     handlers that don't receive a context: the goroutine+select is what
//     keeps the loop responsive; the orphaned call finishing on its own is
//     the cost of that, not a bug to silently paper over. An unbounded
//     module action (matching the worker's own unbounded tier for
//     SERVICE_START and friends -- see CLAUDE.md's "Consume-Loop Watchdog"
//     section) is the honest tradeoff underneath this timeout, not something
//     the timeout pretends to fix.
//
// A package var (not a const) so tests can shorten it, mirroring the pattern
// CLAUDE.md documents for internal/worker's swap-rate knobs ("tests need not
// sleep an hour"); TestLocalToolCallTimeoutDefault pins the shipped value.
var localToolCallTimeout = 5 * time.Minute

// ============================================================================
// Module control tools (#846 reuse)
// ============================================================================

// runModuleControlCaptured drives the exact #846 primitive
// (runModuleControl, cmd/module_control.go) -- same manifest validation, same
// liveModuleOps.Start/Stop, same durable desired_status marker -- through
// captureStdout so its human-readable progress lines (and any `docker
// compose` subprocess output nested inside it, see captureStdout's comment)
// never reach the MCP server's real stdout, which is the JSON-RPC transport.
//
// The capture is tail-truncated to maxCapturedModuleOutputBytes: a
// build-based service's first start (e.g. bonsai, see CLAUDE.md) can emit a
// multi-megabyte `docker compose build` log, and captureStdout itself makes
// no promise about size -- only that collecting it won't hang.
func runModuleControlCaptured(name string, action moduleAction) (string, error) {
	out, err := captureStdout(func() error {
		// citadel#853: runModuleControl gained a ctx parameter (feeds
		// --expect-node's gatherIdentity fallback probe only -- see its doc
		// comment). This tool's own moduleControlFn signature has no ctx (see
		// localToolCallTimeout's doc comment for why threading one through
		// wouldn't actually bound this call), and local_module_stop/start/
		// restart don't expose --expect-node, so there is nothing for a
		// caller-supplied context to cancel here; context.Background() is
		// correct, not a placeholder.
		return runModuleControl(context.Background(), name, action)
	})
	return tailTruncate(out, maxCapturedModuleOutputBytes), err
}

// maxCapturedModuleOutputBytes bounds how much of a module-control command's
// captured output rides in a single JSON-RPC tool result.
const maxCapturedModuleOutputBytes = 16 * 1024 // 16 KiB

// tailTruncate keeps the LAST maxLen bytes of s, marking that truncation
// happened. Deliberately tail- rather than head-truncation (unlike
// truncate() in mcp.go, used for short debug-log previews): for `docker
// compose` output the diagnostically useful text -- the actual error -- is
// at the END of the log, and head-truncating would keep only build/pull
// noise and drop exactly the part an agent needs to see.
func tailTruncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return fmt.Sprintf("...[truncated %d bytes]...\n", len(s)-maxLen) + s[len(s)-maxLen:]
}

// newModuleControlTools builds the local_module_stop/start/restart tools.
// dispatch is injected (production: runModuleControlCaptured) so tests can
// verify tool-argument parsing and dispatch routing with a stub that never
// touches a real manifest or container.
func newModuleControlTools(dispatch moduleControlFn) []localMCPTool {
	call := func(action moduleAction) func(ctx context.Context, args json.RawMessage) (string, error) {
		return func(ctx context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Name string `json:"name"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return "", fmt.Errorf("invalid arguments: %w", err)
				}
			}
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return "", fmt.Errorf("'name' is required (the module/service name as it appears in citadel.yaml)")
			}
			return dispatch(name, action)
		}
	}

	nameSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": `Module/service name as it appears in citadel.yaml (e.g. "vllm", "bonsai").`,
			},
		},
		"required": []string{"name"},
	}

	return []localMCPTool{
		{
			Name: "local_module_stop",
			Description: "Stop a single module/service on THIS node by name. Scoped: no other " +
				"module is touched and the citadel worker is not restarted. Sets the durable " +
				"desired_status:stopped marker so the stop survives a reboot. LOCAL authority: " +
				"runs on this node for the node owner, no AceTeam platform round-trip, no " +
				"node:modules scope required. Already-stopped is a clean no-op success.",
			InputSchema: nameSchema,
			Call:        call(moduleActionStop),
		},
		{
			Name: "local_module_start",
			Description: "Start a single module/service on THIS node by name. Scoped: no other " +
				"module is touched and the citadel worker is not restarted. Clears the durable " +
				"desired_status:stopped marker so it also starts again on the next boot. LOCAL " +
				"authority: runs on this node for the node owner, no AceTeam platform round-trip, " +
				"no node:modules scope required. Already-running is a clean no-op success.",
			InputSchema: nameSchema,
			Call:        call(moduleActionStart),
		},
		{
			Name: "local_module_restart",
			Description: "Restart a single module/service on THIS node by name -- equivalent to " +
				"local_module_stop immediately followed by local_module_start. Scoped: no other " +
				"module is touched. LOCAL authority: no AceTeam platform round-trip, no " +
				"node:modules scope required.",
			InputSchema: nameSchema,
			Call:        call(moduleActionRestart),
		},
	}
}

// ============================================================================
// Local inference tools (gateway chat-route reuse)
// ============================================================================

// maxLocalChatResponseBytes bounds how much of a local engine's response body
// this tool buffers into a single JSON-RPC tool result.
const maxLocalChatResponseBytes = 1 << 20 // 1 MiB

// localChatMessage mirrors mesh.ChatMessage's JSON shape for tool-argument
// decoding (kept local rather than aliasing mesh.ChatMessage directly so this
// file's wire schema doesn't silently change if that struct's tags ever do).
type localChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// newLocalInferenceTools builds local_list_models and local_chat.
func newLocalInferenceTools(deps localMCPDeps) []localMCPTool {
	listModels := localMCPTool{
		Name: "local_list_models",
		Description: "List models currently served by LOCAL inference engines on this node " +
			"(vLLM, llama.cpp, bonsai, Ollama), with the engine name and host port for each. " +
			"LOCAL authority: reads this node's own engine discovery, no central platform " +
			"round-trip, no mesh probe of other nodes.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return localListModelsCall(deps)
		},
	}

	chat := localMCPTool{
		Name: "local_chat",
		Description: "Send a chat-completion request directly to a model served LOCALLY on " +
			"this node's own engine (vLLM/llama.cpp/bonsai/Ollama), dialing the engine's " +
			"citadel-owned host port on 127.0.0.1 directly -- no central Redis job queue, no " +
			"mesh hop, no other node involved. Provide 'prompt' for a single-turn user message, " +
			"or 'messages' for multi-turn context. Always non-streaming (returns the full " +
			"response in one result). Call local_list_models first to see what is available.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model": map[string]any{
					"type":        "string",
					"description": "Model id as reported by local_list_models. May be omitted only if this node serves exactly one model.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "Single user message. Shorthand for messages:[{role:\"user\",content:prompt}]. Ignored if 'messages' is set.",
				},
				"messages": map[string]any{
					"type":        "array",
					"description": "OpenAI-style chat messages [{role, content}]. Use instead of 'prompt' for multi-turn context.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role":    map[string]any{"type": "string"},
							"content": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return localChatCall(ctx, deps, args)
		},
	}

	return []localMCPTool{listModels, chat}
}

func localListModelsCall(deps localMCPDeps) (string, error) {
	if deps.chatLister == nil {
		return "", fmt.Errorf("local engine discovery is not available")
	}
	type modelInfo struct {
		Engine string   `json:"engine"`
		Port   int      `json:"port"`
		Models []string `json:"models"`
	}
	engines := deps.chatLister()
	out := make([]modelInfo, 0, len(engines))
	for _, e := range engines {
		out = append(out, modelInfo{Engine: e.Engine, Port: e.Port, Models: e.Models})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func localChatCall(ctx context.Context, deps localMCPDeps, args json.RawMessage) (string, error) {
	if deps.chatLister == nil || deps.chatClient == nil {
		return "", fmt.Errorf("local inference routing is not available")
	}

	var in struct {
		Model    string             `json:"model"`
		Prompt   string             `json:"prompt"`
		Messages []localChatMessage `json:"messages"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if strings.TrimSpace(in.Prompt) == "" && len(in.Messages) == 0 {
		return "", fmt.Errorf("either 'prompt' or 'messages' is required")
	}

	engines := deps.chatLister()
	port, engine, ok := gateway.ResolveChatModel(in.Model, engines)
	if !ok {
		if strings.TrimSpace(in.Model) == "" {
			return "", fmt.Errorf("no model specified and this node's serving engines are ambiguous or empty; call local_list_models and pass an explicit 'model'")
		}
		return "", fmt.Errorf("model %q is not served locally on this node; call local_list_models to see what is available", in.Model)
	}

	// Always non-streaming: a tools/call result is a single JSON-RPC response,
	// not a stream, so there is nothing to gain from SSE here and it would only
	// need buffering/parsing back into one string anyway.
	var body []byte
	var err error
	if len(in.Messages) > 0 {
		msgs := make([]mesh.ChatMessage, 0, len(in.Messages))
		for _, m := range in.Messages {
			msgs = append(msgs, mesh.ChatMessage{Role: m.Role, Content: m.Content})
		}
		body, err = json.Marshal(mesh.ChatRequest{Model: in.Model, Messages: msgs, Stream: false})
	} else {
		body, err = mesh.BuildChatRequest(in.Model, in.Prompt, false)
	}
	if err != nil {
		return "", fmt.Errorf("build chat request: %w", err)
	}

	resp, err := deps.chatClient.ChatCompletion(ctx, "127.0.0.1", port, body)
	if err != nil {
		return "", fmt.Errorf("local engine %s (port %d) unreachable: %w", engine, port, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalChatResponseBytes))
	if err != nil {
		return "", fmt.Errorf("read response from engine %s: %w", engine, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("engine %s returned HTTP %d: %s", engine, resp.StatusCode, truncate(string(data), 500))
	}
	return string(data), nil
}

// ============================================================================
// Local file tools (FILE_READ/FILE_LIST handler reuse)
// ============================================================================

// localJobLogFn discards handler log lines. The FILE_READ/FILE_LIST handlers'
// only stdout write (jobs.JobContext.Log's nil-LogFn fallback) is a single
// informational line; a tools/call result should carry only the tool's own
// output, and (as with module control) the MCP server's real stdout is the
// JSON-RPC transport, so this must never be nil.
func localJobLogFn(string, string) {}

// newLocalFileTools builds local_read_file and local_list_files, both
// re-executing the SAME job handlers (internal/jobs.FileReadHandler /
// FileListHandler) the FILE_READ/FILE_LIST job types use, sandboxed to
// workspaceDir via jobs.ValidateReadPath with AllowOutsideWorkspace left at
// its zero value (false) -- no arbitrary filesystem access, read-only, and
// this cannot silently diverge from what a dispatched FILE_READ/FILE_LIST job
// would allow because it is the identical code path.
func newLocalFileTools(workspaceDir string) []localMCPTool {
	readFile := localMCPTool{
		Name: "local_read_file",
		Description: "Read a file from this node's workspace directory. Sandboxed: the path " +
			"must resolve inside the workspace (symlink escapes are rejected); there is no " +
			"arbitrary filesystem access. Read-only. Returns content with 1-based line numbers, " +
			"like `cat -n`.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path relative to the node workspace (or absolute, but it must resolve inside the workspace).",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "0-based starting line (default 0).",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max lines to return (default 2000).",
				},
			},
			"required": []string{"path"},
		},
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return localReadFileCall(workspaceDir, args)
		},
	}

	listFiles := localMCPTool{
		Name: "local_list_files",
		Description: "List directory contents within this node's workspace directory. " +
			"Sandboxed the same way as local_read_file. Read-only.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": `Directory path relative to the node workspace (default "." -- the workspace root).`,
				},
				"pattern": map[string]any{
					"type":        "string",
					"description": `Optional glob pattern to filter entries, e.g. "*.go".`,
				},
			},
		},
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return localListFilesCall(workspaceDir, args)
		},
	}

	return []localMCPTool{readFile, listFiles}
}

func localReadFileCall(workspaceDir string, args json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset *int   `json:"offset"`
		Limit  *int   `json:"limit"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if strings.TrimSpace(in.Path) == "" {
		return "", fmt.Errorf("'path' is required")
	}

	payload := map[string]string{"path": in.Path}
	if in.Offset != nil {
		payload["offset"] = strconv.Itoa(*in.Offset)
	}
	if in.Limit != nil {
		payload["limit"] = strconv.Itoa(*in.Limit)
	}

	handler := jobs.NewFileReadHandler(workspaceDir)
	out, err := handler.Execute(jobs.JobContext{LogFn: localJobLogFn}, &nexus.Job{ID: "mcp-local", Payload: payload})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func localListFilesCall(workspaceDir string, args json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		path = "."
	}
	payload := map[string]string{"path": path}
	if in.Pattern != "" {
		payload["pattern"] = in.Pattern
	}

	handler := jobs.NewFileListHandler(workspaceDir)
	out, err := handler.Execute(jobs.JobContext{LogFn: localJobLogFn}, &nexus.Job{ID: "mcp-local", Payload: payload})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ============================================================================
// Model exclusivity tools (aceteam#8248/#8249 v2, reusing internal/jobs's
// Reserve/ReserveExclusive/Release/StartServiceWithModel -- see
// docs/design-model-exclusivity.md and internal/jobs/model_exclusivity.go's
// package doc for the full design, including the crash-safety shape these
// tools inherit)
// ============================================================================

// localDeployFn pulls (if model is non-empty) then starts serviceName
// serving model with an optional VRAM budget. jobID is used as the
// MODEL_CACHE_PULL job id (cosmetic/logging only when this is a plain,
// non-exclusive deploy) or the reservation's deterministic id (when called
// as the start-half of local_run_exclusive, after eviction already ran).
type localDeployFn func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error)

// localReserveExclusiveFn evicts every non-pinned running service (except
// exclude) unconditionally, tagging each with jobID. Mirrors
// internal/jobs.ServiceHandler.ReserveExclusive's return shape (Evicted,
// Reason, error) without exposing *jobs.Reservation to the tool layer, so a
// test stub needs no dependency on internal/jobs at all.
type localReserveExclusiveFn func(jobID, exclude string) (evicted []string, reason string, err error)

// localReserveBudgetFn is the bounded (vram_mb) alternative: an ordinary,
// satisfiable internal/jobs.ServiceHandler.Reserve against an explicit
// budget rather than "evict everything".
type localReserveBudgetFn func(jobID string, requiredVRAMBytes uint64) (evicted []string, reason string, err error)

// localReleaseFn restores every service tagged evicted_by_job==jobID.
type localReleaseFn func(jobID string) (restored []string, err error)

// localHasReservationFn reports whether jobID currently holds an active
// reservation -- used by local_model_stop to decide whether stopping the
// model it served should ALSO release a reservation.
type localHasReservationFn func(jobID string) (bool, error)

// localReservationOps groups the #8248/#8249 v2 model-exclusivity operations
// behind simple function types, mirroring localMCPDeps.moduleControl's
// existing injection pattern exactly: tests stub each independently without
// ever constructing a real internal/jobs.ServiceHandler.
type localReservationOps struct {
	deploy               localDeployFn
	reserveExclusive     localReserveExclusiveFn
	reserveBudget        localReserveBudgetFn
	release              localReleaseFn
	hasActiveReservation localHasReservationFn
}

// realLocalReservationOps builds the production localReservationOps. Each
// closure resolves the manifest config dir fresh (via findOrCreateManifest,
// --node-dir-aware) and constructs a jobs.ServiceHandler per call --
// deliberately not shared/cached across calls, mirroring how
// runModuleControlCaptured re-resolves the manifest on every invocation
// rather than assuming it hasn't changed between two tool calls in the same
// `citadel mcp` process.
func realLocalReservationOps() localReservationOps {
	resolve := func() (*jobs.ServiceHandler, error) {
		if err := refuseIfReservationNodeDirUnsupported("citadel mcp (local_model_deploy/local_run_exclusive/local_model_stop)"); err != nil {
			return nil, err
		}
		_, configDir, err := findOrCreateManifest()
		if err != nil {
			return nil, err
		}
		return jobs.NewServiceHandlerWithWorkspace(configDir, resolveWorkspaceDir()), nil
	}
	jctx := jobs.JobContext{LogFn: localJobLogFn}

	return localReservationOps{
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			h, err := resolve()
			if err != nil {
				return "", err
			}
			if model != "" {
				pull := &jobs.ModelCachePullHandler{}
				if _, pullErr := pull.Execute(jctx, &nexus.Job{ID: jobID, Payload: map[string]string{"model_name": model, "engine": serviceName}}); pullErr != nil {
					return "", fmt.Errorf("pull model %q for %q: %w", model, serviceName, pullErr)
				}
			}
			out, startErr := h.StartServiceWithModel(jctx, serviceName, model, requiredVRAMBytes)
			if startErr != nil {
				return "", startErr
			}
			return string(out), nil
		},
		reserveExclusive: func(jobID, exclude string) ([]string, string, error) {
			h, err := resolve()
			if err != nil {
				return nil, "", err
			}
			res, resErr := h.ReserveExclusive(jctx, jobID, exclude)
			if res == nil {
				return nil, "", resErr
			}
			return res.Evicted, res.Reason, resErr
		},
		reserveBudget: func(jobID string, requiredVRAMBytes uint64) ([]string, string, error) {
			h, err := resolve()
			if err != nil {
				return nil, "", err
			}
			res, resErr := h.Reserve(jctx, jobID, requiredVRAMBytes)
			if res == nil {
				return nil, "", resErr
			}
			return res.Evicted, res.Reason, resErr
		},
		release: func(jobID string) ([]string, error) {
			h, err := resolve()
			if err != nil {
				return nil, err
			}
			return h.Release(jctx, jobID)
		},
		hasActiveReservation: func(jobID string) (bool, error) {
			h, err := resolve()
			if err != nil {
				return false, err
			}
			return h.HasActiveReservation(jobID)
		},
	}
}

// vramMBToBytes converts an optional vram_mb tool argument to bytes, treating
// a non-positive value as "no budget declared" (0) -- mirrors
// internal/jobs.parseRequiredVRAMBytes's identical fail-safe-on-absent-signal
// contract.
func vramMBToBytes(mb float64) uint64 {
	if mb <= 0 {
		return 0
	}
	return uint64(mb * 1024 * 1024)
}

// newModelExclusivityTools builds local_model_deploy, local_run_exclusive,
// and local_model_stop.
func newModelExclusivityTools(deps localMCPDeps) []localMCPTool {
	engineDesc := `Target managed service (e.g. "vllm", "bonsai", "llamacpp", "ollama"). Required -- ` +
		`this tool does not guess an engine from the model name; call local_list_models to see what ` +
		`this node can serve.`

	deploy := localMCPTool{
		Name: "local_model_deploy",
		Description: "Pull (if needed) and start a model on THIS node's own engine. Optionally bounds " +
			"VRAM via vram_mb, which may durably stop OTHER non-pinned services to fit " +
			"(citadel-cli#577's ordinary preemption -- NOT a reservation: nothing is automatically " +
			"restored later). For a reservation that restores evicted peers when you're done, use " +
			"local_run_exclusive instead. LOCAL authority: runs on this node for the node owner, no " +
			"AceTeam platform round-trip, no node:modules scope required.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model":   map[string]any{"type": "string", "description": "Model id/repo to deploy."},
				"engine":  map[string]any{"type": "string", "description": engineDesc},
				"vram_mb": map[string]any{"type": "number", "description": "Optional VRAM budget in MB. May durably stop other non-pinned services to fit (not a reservation -- see local_run_exclusive for that)."},
			},
			"required": []string{"model", "engine"},
		},
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return localModelDeployCall(deps, args)
		},
	}

	exclusive := localMCPTool{
		Name: "local_run_exclusive",
		Description: "Reserve THIS node's GPU exclusively for one model: durably evict every other " +
			"non-pinned service (or, with vram_mb, reserve exactly that many MB instead), then pull " +
			"(if needed) and start 'engine' serving 'model'. The reservation is NOT auto-released -- " +
			"call local_model_stop with the SAME model once you're done, which restores every evicted " +
			"peer. If this MCP session ends without calling local_model_stop, the reservation stays " +
			"held until either an explicit 'citadel module reservations release exclusive:<engine>' or " +
			"the next 'citadel work' boot on this node (which restores an orphaned reservation " +
			"automatically) -- a `citadel work` that boots WHILE this reservation is still legitimately " +
			"active can also mistake it for orphaned and restore the evicted peers prematurely; see " +
			"docs/design-model-exclusivity.md for the full crash-safety contract. Always returns the " +
			"full list of evicted services so you can see the blast radius before relying on it.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model":   map[string]any{"type": "string", "description": "Model id/repo to deploy exclusively."},
				"engine":  map[string]any{"type": "string", "description": engineDesc + ` Also determines the reservation id ("exclusive:<engine>").`},
				"vram_mb": map[string]any{"type": "number", "description": "Optional: reserve exactly this many MB instead of evicting every non-pinned service."},
			},
			"required": []string{"model", "engine"},
		},
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return localRunExclusiveCall(deps, args)
		},
	}

	stop := localMCPTool{
		Name: "local_model_stop",
		Description: "Stop the engine currently serving 'model' on THIS node (resolved via the same " +
			"lookup local_list_models uses). If 'model' was deployed via local_run_exclusive, this ALSO " +
			"releases that reservation, restoring every service it evicted -- this IS the paired " +
			"release call for local_run_exclusive, not a second, separate tool. Distinct from the " +
			"platform's MODEL_CACHE_EVICT job type, which deletes cached weights from disk -- this " +
			"tool only stops serving; nothing is deleted. LOCAL authority: no AceTeam platform " +
			"round-trip, no node:modules scope required.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"model": map[string]any{"type": "string", "description": "Model id as reported by local_list_models."},
			},
			"required": []string{"model"},
		},
		Call: func(ctx context.Context, args json.RawMessage) (string, error) {
			return localModelStopCall(deps, args)
		},
	}

	return []localMCPTool{deploy, exclusive, stop}
}

func localModelDeployCall(deps localMCPDeps, args json.RawMessage) (string, error) {
	var in struct {
		Model  string  `json:"model"`
		Engine string  `json:"engine"`
		VRAMMB float64 `json:"vram_mb"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	model := strings.TrimSpace(in.Model)
	engine := strings.TrimSpace(in.Engine)
	if model == "" {
		return "", fmt.Errorf("'model' is required")
	}
	if engine == "" {
		return "", fmt.Errorf("'engine' is required (the target managed service, e.g. \"vllm\"); not inferred from the model name")
	}
	if deps.reservations.deploy == nil {
		return "", fmt.Errorf("model deploy is not available")
	}
	return deps.reservations.deploy("mcp-local-deploy", engine, model, vramMBToBytes(in.VRAMMB))
}

func localRunExclusiveCall(deps localMCPDeps, args json.RawMessage) (string, error) {
	var in struct {
		Model  string  `json:"model"`
		Engine string  `json:"engine"`
		VRAMMB float64 `json:"vram_mb"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	model := strings.TrimSpace(in.Model)
	engine := strings.TrimSpace(in.Engine)
	if model == "" {
		return "", fmt.Errorf("'model' is required")
	}
	if engine == "" {
		return "", fmt.Errorf("'engine' is required (the target managed service, e.g. \"vllm\"); not inferred from the model name")
	}
	if deps.reservations.deploy == nil {
		return "", fmt.Errorf("exclusive model run is not available")
	}

	jobID := jobs.ExclusiveReservationJobID(engine)
	var evicted []string
	var reason string
	var err error
	if in.VRAMMB > 0 {
		if deps.reservations.reserveBudget == nil {
			return "", fmt.Errorf("bounded (vram_mb) reservation is not available")
		}
		evicted, reason, err = deps.reservations.reserveBudget(jobID, vramMBToBytes(in.VRAMMB))
	} else {
		if deps.reservations.reserveExclusive == nil {
			return "", fmt.Errorf("exclusive reservation is not available")
		}
		evicted, reason, err = deps.reservations.reserveExclusive(jobID, engine)
	}
	if err != nil {
		return "", fmt.Errorf("reserve GPU for %q: %w (evicted before the failure: %s)", engine, err, strings.Join(evicted, ", "))
	}

	startOut, startErr := deps.reservations.deploy(jobID, engine, model, 0)
	if startErr != nil {
		return "", fmt.Errorf("reservation %s is HELD (evicted: %s) but starting %q failed: %w -- "+
			"call local_model_stop(model=%q) or 'citadel module reservations release %s' to restore the evicted peers",
			jobID, strings.Join(evicted, ", "), engine, startErr, model, jobID)
	}

	result := map[string]any{
		"reservation_id": jobID,
		"evicted":        evicted,
		"reason":         reason,
		"start_result":   json.RawMessage(startOut),
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func localModelStopCall(deps localMCPDeps, args json.RawMessage) (string, error) {
	if deps.chatLister == nil {
		return "", fmt.Errorf("local engine discovery is not available")
	}
	var in struct {
		Model string `json:"model"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return "", fmt.Errorf("'model' is required")
	}

	engines := deps.chatLister()
	_, engine, ok := gateway.ResolveChatModel(model, engines)
	if !ok {
		return "", fmt.Errorf("model %q is not served locally on this node; call local_list_models to see what is available", model)
	}
	if deps.moduleControl == nil {
		return "", fmt.Errorf("module control is not available")
	}

	// Stop the target FIRST, then release any reservation it held. The
	// reverse order would restart evicted peers while the target still holds
	// its own VRAM, which can fail to fit or needlessly contend for it.
	stopOut, stopErr := deps.moduleControl(engine, moduleActionStop)
	if stopErr != nil {
		return "", fmt.Errorf("stop %q (serving %q): %w", engine, model, stopErr)
	}

	result := map[string]any{
		"engine":      engine,
		"stop_result": stopOut,
	}
	jobID := jobs.ExclusiveReservationJobID(engine)
	if deps.reservations.hasActiveReservation != nil {
		if has, hasErr := deps.reservations.hasActiveReservation(jobID); hasErr == nil && has && deps.reservations.release != nil {
			restored, relErr := deps.reservations.release(jobID)
			result["reservation_id"] = jobID
			result["restored"] = restored
			if relErr != nil {
				result["release_error"] = relErr.Error()
			}
		}
	}

	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ============================================================================
// stdout capture (protects the JSON-RPC transport from CLI-oriented helpers)
// ============================================================================

// captureStdout temporarily redirects the process's os.Stdout to a pipe while
// fn runs, and returns everything written to it as a string.
//
// Why this exists: runModuleControl (and the #846 primitives it calls) is a
// CLI helper that prints human-readable progress with fmt.Printf, which
// writes to os.Stdout. Worse, the actual `docker compose up|down` subprocess
// it shells out to (cmd/service.go:startService, cmd/stop.go:
// stopServiceByCompose) hardwires cmd.Stdout = os.Stdout too -- so a plain
// io.Writer parameter threaded through the Go call chain would NOT be
// enough; the subprocess's own stdout has to be redirected as well. Swapping
// the process-wide os.Stdout for the duration of the call is the only way to
// catch both. Under `citadel mcp`, os.Stdout is the JSON-RPC transport
// (cmd/mcp.go), so anything that leaks onto it corrupts the protocol for the
// rest of the session -- this is not a cosmetic concern.
//
// Safe to do here because the MCP stdio loop (mcpBridge.run) is
// single-threaded and fully synchronous: it reads one line, handles it
// completely, and only then reads the next -- so under normal operation there
// is exactly one os.Stdout redirection in flight at a time. That invariant
// stopped being automatic once citadel#858 gave local tool calls a REAL
// timeout (callLocalToolWithTimeout, cmd/mcp.go): a call that blows its
// deadline is abandoned, not killed, so its goroutine can still be in here,
// holding os.Stdout redirected to its own pipe, when the loop moves on and
// dispatches a SECOND local tool call. Two concurrent redirections stomping
// on the same os.Stdout variable would corrupt it for good (whichever
// finishes last "wins" the restore, and the loser's saved prevStdout was
// never the real stdout). stdoutCaptureInFlight below closes that gap by
// refusing to nest -- fail fast with a clear error instead of blocking (a
// mutex here would just reintroduce the wedge #858 fixes) or silently
// racing.
//
// The read side is drained by a background goroutine for the ENTIRE
// redirection window (started before fn runs, read until EOF after the pipe
// writer is closed), so this cannot deadlock regardless of how much output fn
// or a subprocess it starts produces -- including a multi-megabyte first-time
// build log (e.g. bonsai's inline `docker compose build`, see CLAUDE.md). The
// caller is still responsible for bounding what it does with a very large
// capture (see runModuleControlCaptured's tailTruncate call); this helper
// only guarantees it won't hang collecting it.
//
// This also depends on fn only starting subprocesses SYNCHRONOUSLY (i.e. via
// exec.Cmd.Run, as startService/stopServiceByCompose do today) so that fn
// does not return until the subprocess has exited and its dup of the pipe
// write end is closed. A future fn that used exec.Cmd.Start (backgrounding
// the subprocess) would return while the child still held that fd open, so
// closing w here would not be enough to unblock the reader goroutine's
// io.ReadAll -- it would keep waiting for the CHILD's copy to close too,
// i.e. potentially forever. Anything added to this call chain must stay
// synchronous.
//
// Also note there is no cancellation here: fn runs to completion regardless
// of any context a caller might have wanted to bound it with, because the
// pipe swap can only be safely undone once fn (and everything it started
// synchronously) has actually returned -- see localToolCallTimeout's comment
// for why this specifically affects the module-control tools.

// stdoutCaptureInFlight is the nesting guard described in captureStdout's
// doc comment. It is released only once os.Stdout has actually been fully
// restored (registered before the restore defer below, so LIFO ordering runs
// it last), so a new capture can never start while a previous one -- even an
// abandoned, still-running one -- is mid-redirection.
//
// The refusal below can persist for as long as the orphaned call keeps
// running -- potentially minutes for a slow `docker compose` action (a
// build-based service's first start, e.g. bonsai, can take ~7min -- see
// CLAUDE.md's Bonsai section). That is still strictly better than the
// pre-#858 behavior (the ENTIRE JSON-RPC loop wedged for that whole window),
// but it is not instantaneous: a caller that retries a refused local tool
// call in a tight loop should back off, not spin.
var stdoutCaptureInFlight atomic.Bool

func captureStdout(fn func() error) (out string, err error) {
	if !stdoutCaptureInFlight.CompareAndSwap(false, true) {
		// fn is never invoked on this path -- nothing was started, changed, or
		// partially applied. Say so explicitly: a caller (an agent driving
		// local_module_stop/start/restart) needs to know the module's state is
		// UNCHANGED, not guess whether the stop/start partially ran.
		return "", fmt.Errorf(
			"mcp: no action was taken -- another local tool's stdout capture is " +
				"still in progress (a previous module-control call likely timed " +
				"out and is still finishing in the background); try again shortly")
	}
	defer stdoutCaptureInFlight.Store(false)

	r, w, perr := os.Pipe()
	if perr != nil {
		// Fail closed: never call fn with the real os.Stdout still wired in, or
		// its output would corrupt the JSON-RPC transport.
		return "", fmt.Errorf("mcp: cannot capture stdout for local tool: %w", perr)
	}

	prevStdout := os.Stdout
	os.Stdout = w

	outCh := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		outCh <- data
	}()

	// Deferred so the restore (and the paired pipe-close/drain-wait cleanup)
	// runs on EVERY exit path, including a panic inside fn -- not just the
	// normal return this used to rely on. A panic here still propagates (this
	// does not recover), so a wedge in fn still crashes the process rather
	// than silently continuing with corrupted JSON-RPC framing (see this
	// function's doc comment on "fail closed"); the defer only guarantees
	// os.Stdout points back at the real transport before that happens, so a
	// recover()ing caller (or a panic that unwinds further before the process
	// exits) never observes a still-redirected os.Stdout.
	//
	// Order matters for the no-deadlock guarantee documented above: restore
	// os.Stdout, THEN close w (which is what lets the reader goroutine's
	// io.ReadAll observe EOF and return), THEN wait for it to actually send on
	// outCh before closing r.
	defer func() {
		os.Stdout = prevStdout
		_ = w.Close()
		out = string(<-outCh)
		_ = r.Close()
	}()

	err = fn()
	return
}
