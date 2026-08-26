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
	}
}

// newLocalMCPTools builds the full local tool set from the given
// dependencies. Kept separate from realLocalMCPDeps so tests can build the
// tool set from a stubbed localMCPDeps without touching runMCP at all.
func newLocalMCPTools(deps localMCPDeps) []localMCPTool {
	tools := newModuleControlTools(deps.moduleControl)
	tools = append(tools, newLocalInferenceTools(deps)...)
	tools = append(tools, newLocalFileTools(deps.workspaceDir)...)
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

// localToolCallTimeout bounds how long a tool that OBSERVES a context may
// run -- concretely, today, only local_chat's HTTP call
// (deps.chatClient.ChatCompletion honors ctx). It does NOT bound
// local_module_stop/start/restart: moduleControlFn's signature has no ctx
// parameter, because the primitive it drives (runModuleControl ->
// liveModuleOps.Start/Stop -> `docker compose up|down` via a synchronous
// exec.Cmd.Run()) has no cancellation hook to wire one into, and threading a
// context through it here would not actually bound the call -- the pipe swap
// in captureStdout can only be undone by the subprocess itself exiting (see
// its doc comment), so a "timeout" that let tool.Call return early would
// leave os.Stdout redirected while runModuleControl kept running in the
// background, corrupting every subsequent JSON-RPC response. An unbounded
// module action (matching the worker's own unbounded tier for SERVICE_START
// and friends -- see CLAUDE.md's "Consume-Loop Watchdog" section) is the
// honest tradeoff, not a bug to silently paper over with an inert deadline.
const localToolCallTimeout = 5 * time.Minute

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
// Safe to do here specifically because the MCP stdio loop (mcpBridge.run) is
// single-threaded and fully synchronous: it reads one line, handles it
// completely (including any local tool call), and only then reads the next.
// Nothing else in a `citadel mcp` process writes to stdout concurrently, so
// there is exactly one os.Stdout redirection in flight at a time.
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
func captureStdout(fn func() error) (string, error) {
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

	fnErr := fn()

	os.Stdout = prevStdout
	_ = w.Close()
	captured := <-outCh
	_ = r.Close()

	return string(captured), fnErr
}
