package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// Regression tests for the workflow Shell-node gate (citadel-cli#1305): a
// workflow is a second way to run a host command, so it must honor the SAME
// shell gate as SHELL_COMMAND (live `shell` permission + node passcode) on
// every entry point. Each refusal test proves the command never ran (a marker
// file stays absent), not just that an error came back.

const (
	testPerNodeQueue = "jobs:v1:shell:org_test:node:n1"
	testSharedQueue  = "jobs:v1:org_test"
	testPasscode     = "pc-7f3e9a-not-in-timestamps"
)

// openShellGate admits every command. Node-level tests pair it with
// shellAuthorizedCtx to exercise the Shell executor past the gate.
func openShellGate() jobs.ShellGate {
	return jobs.ShellGate{
		Enabled:        func() bool { return true },
		HasPasscode:    func() bool { return true },
		VerifyPasscode: func(string) bool { return true },
	}
}

func shellAuthorizedCtx() context.Context { return withShellAuthorized(context.Background()) }

// passcodeGate is an enabled gate with a configured passcode of testPasscode.
func passcodeGate(enabled bool) jobs.ShellGate {
	return jobs.ShellGate{
		Enabled:        func() bool { return enabled },
		HasPasscode:    func() bool { return true },
		VerifyPasscode: func(pin string) bool { return pin == testPasscode },
	}
}

// shellGraph is input -> Shell(command) -> output.
func shellGraph(command string) *WorkflowGraph {
	return &WorkflowGraph{
		InputNode:  &Node{ID: "in", Type: NodeTypeInput},
		OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		InnerNodes: []*Node{{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": command}}},
		Edges: []*Edge{
			{SourceID: "in", SourceKey: "x", TargetID: "sh", TargetKey: "x"},
			{SourceID: "sh", SourceKey: "stdout", TargetID: "out", TargetKey: "stdout"},
		},
	}
}

// markerCommand returns a command that creates a file, and the file's path.
func markerCommand(t *testing.T) (string, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ran")
	return "touch " + marker, marker
}

func assertNotRan(t *testing.T, marker string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond) // a wrongly-started run would have touched it by now
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("shell command ran despite the gate refusing it")
	}
}

// waitTerminal blocks until exec reaches a terminal status. Callers take a
// Snapshot afterwards (returning one here would copy Execution's mutex).
func waitTerminal(t *testing.T, exec *Execution) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		switch exec.Snapshot().Status {
		case StatusCompleted, StatusFailed, StatusCancelled:
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("workflow did not finish")
}

func TestSubmitShellNodeRefusedByGate(t *testing.T) {
	cases := []struct {
		name     string
		gate     jobs.ShellGate
		passcode string
		reason   string
	}{
		{"shell disabled", passcodeGate(false), testPasscode, jobs.ReasonShellDisabled},
		{"no passcode set", jobs.ShellGate{Enabled: func() bool { return true }, HasPasscode: func() bool { return false }}, testPasscode, jobs.ReasonPasscodeNotSet},
		{"wrong passcode", passcodeGate(true), "000000", jobs.ReasonPasscodeInvalid},
		{"absent passcode", passcodeGate(true), "", jobs.ReasonPasscodeInvalid},
		// An executor built without wiring the gate must never run a host command.
		{"unwired zero-value gate", jobs.ShellGate{}, testPasscode, jobs.ReasonPasscodeNotSet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, marker := markerCommand(t)
			e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: tc.gate}})
			exec, err := e.Submit(context.Background(), &RunRequest{Graph: shellGraph(cmd), Passcode: tc.passcode})
			var refusal *jobs.ShellRefusal
			if !errors.As(err, &refusal) || refusal.Reason != tc.reason {
				t.Fatalf("want ShellRefusal %q, got exec=%v err=%v", tc.reason, exec, err)
			}
			if exec != nil || len(e.List()) != 0 {
				t.Fatal("a refused run must not be recorded or started")
			}
			assertNotRan(t, marker)
		})
	}
}

func TestSubmitShellNodeAdmittedWithPermissionAndPasscode(t *testing.T) {
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: passcodeGate(true)}})
	exec, err := e.Submit(context.Background(), &RunRequest{Graph: shellGraph("echo admitted"), Passcode: testPasscode})
	if err != nil {
		t.Fatalf("admitted run refused: %v", err)
	}
	waitTerminal(t, exec)
	snap := exec.Snapshot()
	if snap.Status != StatusCompleted || snap.Output["stdout"] != "admitted" {
		t.Fatalf("want completed with stdout, got %s %v %s", snap.Status, snap.Output, snap.Error)
	}
}

// A graph with no Shell node never consults the gate, so shell-free workflows
// keep working on a node with Shell off and no passcode.
func TestSubmitShellFreeGraphIgnoresGate(t *testing.T) {
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: jobs.ShellGate{}}})
	graph := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		Edges: []*Edge{{SourceID: "in", SourceKey: "x", TargetID: "out", TargetKey: "x"}},
	}
	exec, err := e.Submit(context.Background(), &RunRequest{Graph: graph, Input: map[string]any{"x": 1}})
	if err != nil {
		t.Fatalf("shell-free graph refused: %v", err)
	}
	waitTerminal(t, exec)
	if snap := exec.Snapshot(); snap.Status != StatusCompleted {
		t.Fatalf("want completed, got %s %s", snap.Status, snap.Error)
	}
}

// Defense in depth: the Shell executor refuses a context that did not pass
// Submit's gate, even with an open gate.
func TestShellNodeRefusesUnauthorizedRun(t *testing.T) {
	cmd, marker := markerCommand(t)
	e := NewShellNodeExecutor(ShellConfig{Gate: openShellGate()})
	if _, err := e.Execute(context.Background(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": cmd}}, nil); err == nil {
		t.Fatal("Shell node ran without a gate-authorized run context")
	}
	assertNotRan(t, marker)
}

// Revoking Shell stops the remaining Shell nodes of an already-admitted run.
func TestShellNodeRechecksLivePermission(t *testing.T) {
	cmd, marker := markerCommand(t)
	e := NewShellNodeExecutor(ShellConfig{Gate: passcodeGate(false)})
	_, err := e.Execute(shellAuthorizedCtx(), &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": cmd}}, nil)
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonShellDisabled {
		t.Fatalf("want shell_disabled refusal, got %v", err)
	}
	assertNotRan(t, marker)
}

// Inputs are substituted before the deny/allow checks, so an input value cannot
// carry a denied pattern past them.
func TestShellNodeDenyListSeesSubstitutedInput(t *testing.T) {
	_, marker := markerCommand(t)
	e := NewShellNodeExecutor(ShellConfig{Gate: openShellGate(), DenyList: []string{"touch"}})
	node := &Node{ID: "sh", Type: NodeTypeShell, Params: map[string]any{"command": "echo {{x}}"}}
	if _, err := e.Execute(shellAuthorizedCtx(), node, map[string]any{"x": "hi; touch " + marker}); err == nil {
		t.Fatal("denied pattern smuggled through an input value")
	}
	assertNotRan(t, marker)
}

func TestWorkflowRunJobRefusedOffPerNodeStream(t *testing.T) {
	cmd, marker := markerCommand(t)
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: openShellGate()}})
	graph, _ := json.Marshal(shellGraph(cmd))
	job := &worker.Job{ID: "j1", Type: JobType, SourceQueue: testSharedQueue, Payload: map[string]any{"graph": string(graph), "passcode": testPasscode}}
	result, err := NewHandler(e).Execute(context.Background(), job, &worker.NoOpStreamWriter{})
	if err != nil || result == nil || result.Status != worker.JobStatusFailure {
		t.Fatalf("want failure result off the per-node stream, got %v %v", result, err)
	}
	if len(e.List()) != 0 {
		t.Fatal("a refused job must not start a run")
	}
	assertNotRan(t, marker)
}

func TestWorkflowRunJobShellRefusedWhenShellDisabled(t *testing.T) {
	cmd, marker := markerCommand(t)
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: passcodeGate(false)}})
	graph, _ := json.Marshal(shellGraph(cmd))
	job := &worker.Job{ID: "j2", Type: JobType, SourceQueue: testPerNodeQueue, Payload: map[string]any{"graph": string(graph), "passcode": testPasscode}}
	result, err := NewHandler(e).Execute(context.Background(), job, &worker.NoOpStreamWriter{})
	if err != nil || result == nil || result.Status != worker.JobStatusFailure || result.Output["reason"] != jobs.ReasonShellDisabled {
		t.Fatalf("want shell_disabled failure, got %+v %v", result, err)
	}
	assertNotRan(t, marker)
}

func TestWorkflowRunJobShellAdmittedWithPasscode(t *testing.T) {
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: passcodeGate(true)}})
	graph, _ := json.Marshal(shellGraph("echo job-ok"))
	job := &worker.Job{ID: "j3", Type: JobType, SourceQueue: testPerNodeQueue, Payload: map[string]any{"graph": string(graph), "passcode": testPasscode}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := NewHandler(e).Execute(ctx, job, &worker.NoOpStreamWriter{})
	if err != nil || result == nil || result.Status != worker.JobStatusSuccess {
		t.Fatalf("want success, got %+v %v", result, err)
	}
}

// Even a permissive auth middleware on the plaintext listener cannot start a
// run: /workflow/run is not served there.
func TestPlaintextRunRouteRefusesEvenWithPermissiveAuth(t *testing.T) {
	cmd, marker := markerCommand(t)
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: openShellGate()}})
	mux := http.NewServeMux()
	NewServer(e).RegisterRoutes(mux, noopAuth)
	body, _ := json.Marshal(RunRequest{Graph: shellGraph(cmd), Passcode: testPasscode})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflow/run", bytes.NewReader(body)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403 on the plaintext listener, got %d: %s", w.Code, w.Body.String())
	}
	if len(e.List()) != 0 {
		t.Fatal("plaintext /workflow/run must not start a run")
	}
	assertNotRan(t, marker)
}

func TestControlRunRouteGatedByCoordinator(t *testing.T) {
	cmd, marker := markerCommand(t)
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: openShellGate()}})
	mux := http.NewServeMux()
	blocked := func(http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }
	}
	NewServer(e).RegisterControlRoutes(mux, blocked)
	body, _ := json.Marshal(RunRequest{Graph: shellGraph(cmd), Passcode: testPasscode})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflow/run", bytes.NewReader(body)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("control /workflow/run not gated by the coordinator middleware: got %d", w.Code)
	}
	assertNotRan(t, marker)
}

func TestControlRunRouteShellRefusalIs403WithReason(t *testing.T) {
	cmd, marker := markerCommand(t)
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: passcodeGate(false)}})
	mux := http.NewServeMux()
	NewServer(e).RegisterControlRoutes(mux, noopAuth)
	body, _ := json.Marshal(RunRequest{Graph: shellGraph(cmd), Passcode: testPasscode})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflow/run", bytes.NewReader(body)))
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusForbidden || resp["reason"] != jobs.ReasonShellDisabled {
		t.Fatalf("want 403 shell_disabled, got %d: %s", w.Code, w.Body.String())
	}
	assertNotRan(t, marker)
}

// The passcode is never echoed back through the run record.
func TestPasscodeNotStoredOnExecution(t *testing.T) {
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: passcodeGate(true)}})
	exec, err := e.Submit(context.Background(), &RunRequest{Graph: shellGraph("echo x"), Passcode: testPasscode})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, exec)
	snap := exec.Snapshot()
	b, _ := json.Marshal(&snap)
	if bytes.Contains(b, []byte(testPasscode)) {
		t.Fatalf("passcode leaked into the execution record: %s", b)
	}
}

// Over a real HTTP server, net/http cancels the request context once the 202 is
// written. The run must survive that: a Shell node still running after the
// response must finish rather than be killed by the cancelled request context.
func TestRunSurvivesHTTPRequestCompletion(t *testing.T) {
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: openShellGate()}})
	mux := http.NewServeMux()
	NewServer(e).RegisterControlRoutes(mux, noopAuth)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	body, _ := json.Marshal(RunRequest{Graph: shellGraph("sleep 0.3; echo survived"), Passcode: testPasscode})
	resp, err := ts.Client().Post(ts.URL+"/workflow/run", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var rr RunResponse
	_ = json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	exec := e.Get(rr.ID)
	if exec == nil {
		t.Fatal("run not recorded")
	}
	waitTerminal(t, exec)
	if snap := exec.Snapshot(); snap.Status != StatusCompleted || snap.Output["stdout"] != "survived" {
		t.Fatalf("run did not survive the request: %s %v %s", snap.Status, snap.Output, snap.Error)
	}
}

// The workflow Shell gate shares the node's wrong-passcode throttle: wrong
// passcodes on WORKFLOW_RUN submits start the cooldown, and an absent passcode
// is never counted.
func TestSubmitShellNodeHonorsPasscodeThrottle(t *testing.T) {
	gate := passcodeGate(true)
	gate.Throttle = jobs.NewPasscodeThrottle()
	e := NewExecutor(ExecutorConfig{Shell: ShellConfig{Gate: gate}})
	for i := 0; i < 3*jobs.DefaultPasscodeMaxFailures; i++ {
		if _, err := e.Submit(context.Background(), &RunRequest{Graph: shellGraph("true"), Passcode: ""}); err == nil {
			t.Fatal("an absent passcode must be refused")
		}
	}
	if gate.Throttle.Throttled() {
		t.Fatal("absent passcodes must not start a cooldown")
	}
	for i := 0; i < jobs.DefaultPasscodeMaxFailures; i++ {
		_, _ = e.Submit(context.Background(), &RunRequest{Graph: shellGraph("true"), Passcode: "000000"})
	}
	cmd, marker := markerCommand(t)
	_, err := e.Submit(context.Background(), &RunRequest{Graph: shellGraph(cmd), Passcode: testPasscode})
	var refusal *jobs.ShellRefusal
	if !errors.As(err, &refusal) || refusal.Reason != jobs.ReasonPasscodeThrottled {
		t.Fatalf("after %d wrong passcodes a correct one must be throttled, got %v", jobs.DefaultPasscodeMaxFailures, err)
	}
	assertNotRan(t, marker)
}
