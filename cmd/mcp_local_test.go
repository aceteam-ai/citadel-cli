// cmd/mcp_local_test.go
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/mesh"
)

// ============================================================================
// Tool registration + naming
// ============================================================================

// knownBackendToolNames are names the AceTeam backend's own MCP tool set
// already exposes (docs/mcp-server.md), spot-checked here so a future local
// tool can't accidentally shadow a remote one. See mcp_local.go's package
// comment for why that would be dangerous (acting on the wrong node).
var knownBackendToolNames = []string{
	"service_start", "service_stop", "service_status",
	"code_read", "code_write", "code_edit", "code_list", "code_search",
	"node_module_set", "node_modules_list",
	"fabric_list_nodes", "fabric_node_status", "fabric_dispatch_job",
	"terminal_exec", "terminal_list_nodes",
}

func TestLocalToolNamesDoNotCollideWithKnownBackendTools(t *testing.T) {
	tools := newLocalMCPTools(localMCPDeps{})
	for _, tool := range tools {
		if !strings.HasPrefix(tool.Name, "local_") {
			t.Errorf("tool %q does not use the local_ prefix", tool.Name)
		}
		for _, backend := range knownBackendToolNames {
			if tool.Name == backend {
				t.Errorf("local tool %q collides with a known backend tool name", tool.Name)
			}
		}
	}
}

func TestNewLocalMCPToolsRegistersExpectedTools(t *testing.T) {
	tools := newLocalMCPTools(localMCPDeps{})
	want := []string{
		"local_module_stop", "local_module_start", "local_module_restart",
		"local_list_models", "local_chat",
		"local_read_file", "local_list_files",
		"local_model_deploy", "local_run_exclusive", "local_model_stop",
	}
	got := map[string]bool{}
	for _, tool := range tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema", tool.Name)
		}
		if tool.Call == nil {
			t.Errorf("tool %q has no Call handler", tool.Name)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("missing expected tool %q", name)
		}
	}
	if len(tools) != len(want) {
		t.Errorf("got %d tools, want %d (%v)", len(tools), len(want), tools)
	}
}

// ============================================================================
// Module control tools (#846 reuse via injected moduleControlFn)
// ============================================================================

func TestModuleControlToolsDispatchScopedAction(t *testing.T) {
	type call struct {
		name   string
		action moduleAction
	}
	var calls []call
	stub := func(name string, action moduleAction) (string, error) {
		calls = append(calls, call{name, action})
		return "ok: " + name, nil
	}

	tools := newModuleControlTools(stub)
	stop, ok := findLocalTool(tools, "local_module_stop")
	if !ok {
		t.Fatal("local_module_stop not registered")
	}
	start, ok := findLocalTool(tools, "local_module_start")
	if !ok {
		t.Fatal("local_module_start not registered")
	}
	restart, ok := findLocalTool(tools, "local_module_restart")
	if !ok {
		t.Fatal("local_module_restart not registered")
	}

	if _, err := stop.Call(context.Background(), json.RawMessage(`{"name":"vllm"}`)); err != nil {
		t.Fatalf("stop.Call: %v", err)
	}
	if _, err := start.Call(context.Background(), json.RawMessage(`{"name":"bonsai"}`)); err != nil {
		t.Fatalf("start.Call: %v", err)
	}
	if _, err := restart.Call(context.Background(), json.RawMessage(`{"name":"ollama"}`)); err != nil {
		t.Fatalf("restart.Call: %v", err)
	}

	want := []call{
		{"vllm", moduleActionStop},
		{"bonsai", moduleActionStart},
		{"ollama", moduleActionRestart},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d] = %+v, want %+v", i, calls[i], w)
		}
	}
}

func TestModuleControlToolRequiresName(t *testing.T) {
	stub := func(name string, action moduleAction) (string, error) {
		t.Fatalf("dispatch should not be called without a name, got %q", name)
		return "", nil
	}
	tools := newModuleControlTools(stub)
	stop, _ := findLocalTool(tools, "local_module_stop")

	if _, err := stop.Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error for missing name")
	}
	if _, err := stop.Call(context.Background(), json.RawMessage(`{"name":"  "}`)); err == nil {
		t.Fatal("expected error for blank name")
	}
}

func TestModuleControlToolPropagatesDispatchError(t *testing.T) {
	stub := func(name string, action moduleAction) (string, error) {
		return "", errors.New("compose down failed")
	}
	tools := newModuleControlTools(stub)
	stop, _ := findLocalTool(tools, "local_module_stop")

	_, err := stop.Call(context.Background(), json.RawMessage(`{"name":"vllm"}`))
	if err == nil || !strings.Contains(err.Error(), "compose down failed") {
		t.Fatalf("expected dispatch error to propagate, got %v", err)
	}
}

// TestModuleRestartCallsRestartAction pins the tool-layer wiring only -- the
// restart stop-then-start composition itself is #846's dispatchModuleAction,
// already pinned by TestModuleControlRestartStopsThenStarts in
// module_control_test.go; this proves the MCP tool calls through to it with
// the right action.
func TestModuleRestartCallsRestartAction(t *testing.T) {
	var gotAction moduleAction
	stub := func(name string, action moduleAction) (string, error) {
		gotAction = action
		return "restarted", nil
	}
	tools := newModuleControlTools(stub)
	restart, _ := findLocalTool(tools, "local_module_restart")
	if _, err := restart.Call(context.Background(), json.RawMessage(`{"name":"vllm"}`)); err != nil {
		t.Fatalf("restart.Call: %v", err)
	}
	if gotAction != moduleActionRestart {
		t.Errorf("action = %v, want moduleActionRestart", gotAction)
	}
}

// ============================================================================
// Local inference tools (gateway chat-route reuse)
// ============================================================================

func TestLocalListModelsCall(t *testing.T) {
	deps := localMCPDeps{
		chatLister: func() []gateway.ChatUpstream {
			return []gateway.ChatUpstream{
				{Engine: "bonsai", Port: 8210, Models: []string{"bonsai-27b"}},
			}
		},
	}
	tools := newLocalInferenceTools(deps)
	listModels, ok := findLocalTool(tools, "local_list_models")
	if !ok {
		t.Fatal("local_list_models not registered")
	}
	out, err := listModels.Call(context.Background(), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "bonsai-27b") || !strings.Contains(out, "8210") {
		t.Errorf("unexpected output: %s", out)
	}
}

// newTestChatClient builds a mesh.Client whose dialer redirects every
// connection to srv, mirroring internal/mesh's own
// TestChatCompletionOverInjectedDialer pattern -- so the port/ip the tool
// asks for is irrelevant to where the request actually lands, letting the
// test assert on what the (stand-in local engine) server received.
func newTestChatClient(srv *httptest.Server) *mesh.Client {
	srvAddr := strings.TrimPrefix(srv.URL, "http://")
	dialer := func(ctx context.Context, netw, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srvAddr)
	}
	return mesh.NewClient(dialer)
}

func TestLocalChatRoutesToResolvedEngine(t *testing.T) {
	var gotModel string
	var gotStream bool
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		gotStream = req.Stream
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer srv.Close()

	deps := localMCPDeps{
		chatLister: func() []gateway.ChatUpstream {
			return []gateway.ChatUpstream{
				{Engine: "bonsai", Port: 8210, Models: []string{"bonsai-27b"}},
			}
		},
		chatClient: newTestChatClient(srv),
	}

	tools := newLocalInferenceTools(deps)
	chat, ok := findLocalTool(tools, "local_chat")
	if !ok {
		t.Fatal("local_chat not registered")
	}

	out, err := chat.Call(context.Background(), json.RawMessage(`{"model":"bonsai-27b","prompt":"hello"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `"content":"hi"`) {
		t.Errorf("unexpected output: %s", out)
	}
	if gotModel != "bonsai-27b" {
		t.Errorf("model sent = %q, want bonsai-27b", gotModel)
	}
	if gotStream {
		t.Error("stream was true; local_chat must always force stream:false")
	}
	if gotPath != mesh.ChatEndpointPath {
		t.Errorf("path = %q, want %q", gotPath, mesh.ChatEndpointPath)
	}
}

func TestLocalChatUnknownModelErrors(t *testing.T) {
	deps := localMCPDeps{
		chatLister: func() []gateway.ChatUpstream {
			return []gateway.ChatUpstream{
				{Engine: "bonsai", Port: 8210, Models: []string{"bonsai-27b"}},
			}
		},
		chatClient: mesh.NewClient((&net.Dialer{}).DialContext),
	}
	tools := newLocalInferenceTools(deps)
	chat, _ := findLocalTool(tools, "local_chat")

	_, err := chat.Call(context.Background(), json.RawMessage(`{"model":"does-not-exist","prompt":"hi"}`))
	if err == nil {
		t.Fatal("expected error for unserved model")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the requested model, got: %v", err)
	}
}

func TestLocalChatRequiresPromptOrMessages(t *testing.T) {
	tools := newLocalInferenceTools(localMCPDeps{
		chatLister: func() []gateway.ChatUpstream { return nil },
	})
	chat, _ := findLocalTool(tools, "local_chat")
	_, err := chat.Call(context.Background(), json.RawMessage(`{"model":"x"}`))
	if err == nil {
		t.Fatal("expected error when neither prompt nor messages is set")
	}
}

// ============================================================================
// Local file tools (sandbox to workspace)
// ============================================================================

func TestLocalReadFileWithinWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tools := newLocalFileTools(dir)
	readFile, ok := findLocalTool(tools, "local_read_file")
	if !ok {
		t.Fatal("local_read_file not registered")
	}

	out, err := readFile.Call(context.Background(), json.RawMessage(`{"path":"note.txt"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestLocalReadFileRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tools := newLocalFileTools(dir)
	readFile, _ := findLocalTool(tools, "local_read_file")

	// A relative traversal out of the workspace to a path that actually
	// exists (/etc/passwd on any Linux/macOS CI runner) -- ValidatePath
	// resolves symlinks and falls back to the nearest EXISTING ancestor for
	// nonexistent paths, so a traversal test must target something real or it
	// can fail for the wrong reason.
	if _, err := readFile.Call(context.Background(), json.RawMessage(`{"path":"../../../../../../etc/passwd"}`)); err == nil {
		t.Fatal("expected error for relative path traversal outside workspace")
	}

	// An absolute path outside the workspace must also be rejected --
	// ValidatePath accepts absolute paths syntactically and relies entirely on
	// the within-workspace check to reject them.
	if _, err := readFile.Call(context.Background(), json.RawMessage(`{"path":"/etc/passwd"}`)); err == nil {
		t.Fatal("expected error for absolute path outside workspace")
	}
}

func TestLocalReadFileFailsClosedWithNoWorkspace(t *testing.T) {
	tools := newLocalFileTools("") // resolveWorkspaceDir() failure case
	readFile, _ := findLocalTool(tools, "local_read_file")
	_, err := readFile.Call(context.Background(), json.RawMessage(`{"path":"note.txt"}`))
	if err == nil {
		t.Fatal("expected error when workspace directory is not configured")
	}
}

func TestLocalListFilesDefaultsToWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	tools := newLocalFileTools(dir)
	listFiles, ok := findLocalTool(tools, "local_list_files")
	if !ok {
		t.Fatal("local_list_files not registered")
	}

	out, err := listFiles.Call(context.Background(), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.txt") {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestLocalListFilesPatternFilter(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0644)

	tools := newLocalFileTools(dir)
	listFiles, _ := findLocalTool(tools, "local_list_files")

	out, err := listFiles.Call(context.Background(), json.RawMessage(`{"pattern":"*.go"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "a.go") {
		t.Errorf("expected a.go in output: %s", out)
	}
	if strings.Contains(out, "b.txt") {
		t.Errorf("did not expect b.txt in filtered output: %s", out)
	}
}

// ============================================================================
// tailTruncate
// ============================================================================

// TestLocalToolCallTimeoutDefault pins the shipped default (5 minutes) now
// that localToolCallTimeout is a var (so tests can shorten it) rather than a
// const -- mirroring internal/worker's swap-rate knobs / TestSwapAccountingDefaults.
func TestLocalToolCallTimeoutDefault(t *testing.T) {
	if localToolCallTimeout != 5*time.Minute {
		t.Errorf("localToolCallTimeout = %s, want 5m (if this is an intentional change, update this test too)", localToolCallTimeout)
	}
}

func TestTailTruncateShortStringUnchanged(t *testing.T) {
	if got := tailTruncate("short", 100); got != "short" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestTailTruncateKeepsTail(t *testing.T) {
	s := strings.Repeat("a", 100) + "TAIL-MARKER"
	got := tailTruncate(s, 20)
	if !strings.HasSuffix(got, "TAIL-MARKER") {
		t.Errorf("expected truncated output to keep the tail, got: %s", got)
	}
	if strings.Contains(got, strings.Repeat("a", 100)) {
		t.Error("expected the head to be dropped, but the full head is still present")
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected a truncation marker, got: %s", got)
	}
}

// ============================================================================
// captureStdout
// ============================================================================

func TestCaptureStdoutCapturesWritesAndPropagatesError(t *testing.T) {
	sentinelErr := errors.New("boom")
	out, err := captureStdout(func() error {
		os.Stdout.WriteString("hello from fn\n")
		return sentinelErr
	})
	if !errors.Is(err, sentinelErr) {
		t.Errorf("err = %v, want %v", err, sentinelErr)
	}
	if !strings.Contains(out, "hello from fn") {
		t.Errorf("captured output = %q, missing expected text", out)
	}
}

func TestCaptureStdoutRestoresRealStdout(t *testing.T) {
	before := os.Stdout
	_, _ = captureStdout(func() error {
		if os.Stdout == before {
			t.Error("os.Stdout was not redirected during fn")
		}
		return nil
	})
	if os.Stdout != before {
		t.Error("os.Stdout was not restored after captureStdout returned")
	}
}

// TestCaptureStdoutRestoresOnPanic pins citadel#858's hardening: the restore
// (and paired pipe-close/drain-wait cleanup) must run via defer so it fires
// on EVERY exit path, including a panic inside fn, not just a normal return.
// Before this fix a panic inside fn would leave os.Stdout pointed at the
// capture pipe for the rest of the process's life (fatal under `citadel mcp`,
// where os.Stdout is the JSON-RPC transport) rather than merely crashing
// cleanly. This test recovers the panic itself specifically to observe that
// os.Stdout was restored before the panic finished unwinding this frame --
// captureStdout itself is NOT expected to recover, and does not.
func TestCaptureStdoutRestoresOnPanic(t *testing.T) {
	before := os.Stdout

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected captureStdout to propagate the panic from fn")
			}
		}()
		_, _ = captureStdout(func() error {
			panic("boom")
		})
	}()

	if os.Stdout != before {
		t.Error("os.Stdout was not restored after a panic inside fn")
	}
}

// TestCaptureStdoutRefusesToNestConcurrentCaptures pins the stdoutCaptureInFlight
// guard: citadel#858's real per-call timeout (localToolCallTimeout /
// callLocalToolWithTimeout, cmd/mcp.go) can leave a timed-out local tool
// call's goroutine still running inside captureStdout -- holding os.Stdout
// redirected -- after the caller has already moved on. A second captureStdout
// call starting while that's still true must fail fast rather than starting a
// second os.Stdout redirection (which would corrupt the process-wide
// os.Stdout variable once the two calls' restores race each other -- see
// captureStdout's doc comment).
func TestCaptureStdoutRefusesToNestConcurrentCaptures(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})

	go func() {
		defer close(firstDone)
		_, _ = captureStdout(func() error {
			close(started)
			<-release
			return nil
		})
	}()

	<-started // the first capture now holds stdoutCaptureInFlight

	secondFnRan := false
	_, err := captureStdout(func() error {
		secondFnRan = true
		return nil
	})

	close(release)
	<-firstDone // avoid leaking the goroutine past the test

	if err == nil {
		t.Fatal("expected an error when a capture is already in flight")
	}
	if secondFnRan {
		t.Error("fn must not run while another capture is in flight")
	}
	if os.Stdout == nil {
		t.Error("os.Stdout should never be left nil")
	}
}

// ============================================================================
// Bridge-level wiring: tools/list merge + tools/call local dispatch
// ============================================================================

// countingRoundTripper records how many times the backend was actually
// dialed, so tests can assert "zero backend calls" directly rather than
// inferring it from the response shape (which a graceful local-only fallback
// would produce either way).
type countingRoundTripper struct{ calls int }

func (rt *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	return nil, errors.New("backend should not have been called")
}

func TestBridgeToolsListNoAPIKeyServesLocalOnlyZeroBackendCalls(t *testing.T) {
	rt := &countingRoundTripper{}
	bridge := &mcpBridge{
		apiKey:     "", // no key configured
		apiURL:     "http://backend.invalid",
		mcpServer:  "aceteam",
		httpClient: &http.Client{Transport: rt},
		localTools: []localMCPTool{
			{Name: "local_test_tool", Description: "d", InputSchema: map[string]any{"type": "object"}},
		},
	}

	req := &jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	out, err := captureStdout(func() error {
		bridge.handleToolsList(req)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rt.calls != 0 {
		t.Errorf("backend was called %d times; want 0 when no API key is configured", rt.calls)
	}

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, out)
	}
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0]["name"] != "local_test_tool" {
		t.Errorf("unexpected tools: %+v", resp.Result.Tools)
	}
}

func TestBridgeToolsListMergesBackendAndLocalTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"fabric_list_nodes","description":"d","inputSchema":{"type":"object"}}]}}`))
	}))
	defer srv.Close()

	bridge := &mcpBridge{
		apiKey:     "test-key",
		apiURL:     srv.URL,
		mcpServer:  "aceteam",
		httpClient: srv.Client(),
		localTools: []localMCPTool{
			{Name: "local_test_tool", Description: "d", InputSchema: map[string]any{"type": "object"}},
		},
	}

	req := &jsonRPCRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}
	out, err := captureStdout(func() error {
		bridge.handleToolsList(req)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var resp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, out)
	}

	names := map[string]bool{}
	for _, tool := range resp.Result.Tools {
		names[tool["name"].(string)] = true
	}
	if !names["fabric_list_nodes"] {
		t.Error("expected merged result to include the backend tool")
	}
	if !names["local_test_tool"] {
		t.Error("expected merged result to include the local tool")
	}
}

func TestBridgeToolsCallDispatchesLocalToolWithoutBackend(t *testing.T) {
	rt := &countingRoundTripper{}
	called := false
	bridge := &mcpBridge{
		apiKey:     "test-key",
		httpClient: &http.Client{Transport: rt},
		localTools: []localMCPTool{
			{
				Name: "local_test_tool",
				Call: func(ctx context.Context, args json.RawMessage) (string, error) {
					called = true
					return "ok", nil
				},
			},
		},
	}

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"local_test_tool","arguments":{}}`),
	}

	var handled bool
	out, err := captureStdout(func() error {
		handled = bridge.tryLocalToolsCall(req)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("expected tryLocalToolsCall to handle the local tool name")
	}
	if !called {
		t.Error("local tool's Call was not invoked")
	}
	if rt.calls != 0 {
		t.Errorf("backend was called %d times; want 0 for a local tool", rt.calls)
	}
	if !strings.Contains(out, `"ok"`) {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestBridgeToolsCallFallsThroughForUnknownTool(t *testing.T) {
	bridge := &mcpBridge{localTools: nil}
	req := &jsonRPCRequest{
		Params: json.RawMessage(`{"name":"service_stop","arguments":{}}`),
	}
	if bridge.tryLocalToolsCall(req) {
		t.Error("expected tryLocalToolsCall to return false for a non-local tool name so it falls through to backend forwarding")
	}
}

func TestBridgeToolsCallLocalToolErrorBecomesIsError(t *testing.T) {
	bridge := &mcpBridge{
		localTools: []localMCPTool{
			{
				Name: "local_test_tool",
				Call: func(ctx context.Context, args json.RawMessage) (string, error) {
					return "", errors.New("module not found")
				},
			},
		},
	}
	req := &jsonRPCRequest{
		ID:     json.RawMessage(`1`),
		Method: "tools/call",
		Params: json.RawMessage(`{"name":"local_test_tool","arguments":{}}`),
	}

	out, err := captureStdout(func() error {
		if !bridge.tryLocalToolsCall(req) {
			t.Error("expected local dispatch to handle the request")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, out)
	}
	if !resp.Result.IsError {
		t.Error("expected isError:true")
	}
	if len(resp.Result.Content) == 0 || !strings.Contains(resp.Result.Content[0].Text, "module not found") {
		t.Errorf("unexpected content: %+v", resp.Result.Content)
	}
}
