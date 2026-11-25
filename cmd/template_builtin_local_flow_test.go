package cmd

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/aep"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// Synthetic identities only.
const (
	lfTestNode    = "node-test-1"
	lfTestOrg     = "org-test-1"
	lfTestRunID   = "0f3c2b8e-5a41-4d6b-9c7e-1a2b3c4d5e6f"
	lfTestVersion = "flowver-test-1"
	lfTestImage   = "registry.invalid/local-flow@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	lfTestModel   = "synthetic-model"
	lfTestPort    = 18001
)

// lfDeterministicGraph is canonical already; its digest is pinned below
// against Python's json.dumps(sort_keys=True, separators=(",",":")).
const lfDeterministicGraph = `{"edges":[{"source_id":"one","source_key":"value","target_id":"output","target_key":"answer"}],"inner_nodes":[{"id":"one","params":{"value":1},"type":"ConstantInteger"}],"input_node":{"id":"input","params":{"fields":{}},"type":"Input"},"output_node":{"id":"output","params":{"fields":{"answer":{"type":"integer"}}},"type":"Output"}}`

const lfDeterministicDigest = "sha256:e9da860395ce96a3c5553a5a06027f55daad56be96640223ce4664ff454edf66"

// lfLLMGraph is indented and carries non-ASCII and HTML-sensitive characters,
// so the Go canonicalizer must escape exactly as Python's ensure_ascii does.
const lfLLMGraph = "{\n \"input_node\": {\"id\": \"input\", \"type\": \"Input\", \"params\": {\"fields\": {\"prompt\": {\"type\": \"string\"}}}},\n" +
	" \"inner_nodes\": [{\"id\": \"llm\", \"type\": \"LocalInference\", \"params\": {\"model\": \"synthetic-model\", \"max_tokens\": 32}},\n" +
	"  {\"id\": \"label\", \"type\": \"ConstantString\", \"params\": {\"value\": \"café <ok> & ☃\"}}],\n" +
	" \"output_node\": {\"id\": \"output\", \"type\": \"Output\", \"params\": {\"fields\": {\"response\": {\"type\": \"string\"}, \"label\": {\"type\": \"string\"}}}},\n" +
	" \"edges\": [{\"source_id\": \"input\", \"source_key\": \"prompt\", \"target_id\": \"llm\", \"target_key\": \"prompt\"},\n" +
	"  {\"source_id\": \"llm\", \"source_key\": \"response\", \"target_id\": \"output\", \"target_key\": \"response\"},\n" +
	"  {\"source_id\": \"label\", \"source_key\": \"value\", \"target_id\": \"output\", \"target_key\": \"label\"}]\n}"

const lfLLMDigest = "sha256:3a73033a9ad67d7b7cdbb8d74dedceea4851bdb5fc78bcab563ba5f13564ea99"

// fakeLocalFlowEngine is a fake container runtime: it answers image inspect,
// records run argv and stdin, and returns a canned engine reply.
type fakeLocalFlowEngine struct {
	mu                  sync.Mutex
	imagePresent        bool
	stdout              string
	exitCode            int
	runs                [][]string
	stdins              [][]byte
	removed             []string
	removeErr           error
	containerPresent    bool
	autoRemoveOnFailure bool
}

func (f *fakeLocalFlowEngine) exec(cmd *exec.Cmd, stdin []byte, limit int) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := cmd.Args[1:]
	switch args[0] {
	case "image":
		if !f.imagePresent {
			return nil, false, errors.New("no such image")
		}
		return []byte("sha256:abc\n"), false, nil
	case "rm":
		f.removed = append(f.removed, args[len(args)-1])
		if f.removeErr != nil {
			return nil, false, f.removeErr
		}
		f.containerPresent = false
		return nil, false, nil
	case "ps":
		if f.containerPresent {
			return []byte("citadel-local-flow-test\n"), false, nil
		}
		return nil, false, nil
	case "run":
		f.containerPresent = true
		f.runs = append(f.runs, append([]string(nil), args...))
		f.stdins = append(f.stdins, append([]byte(nil), stdin...))
		out := []byte(f.stdout)
		overflow := len(out) > limit
		if overflow {
			out = out[:limit]
		}
		if f.exitCode != 0 {
			if f.autoRemoveOnFailure {
				f.containerPresent = false
			}
			return out, overflow, fakeExitError(f.exitCode)
		}
		return out, overflow, nil
	}
	return nil, false, fmt.Errorf("unexpected engine command %v", args)
}

// fakeExitError produces a real *exec.ExitError with the given code.
func fakeExitError(code int) error {
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
}

func (f *fakeLocalFlowEngine) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

type lfFixture struct {
	t       *testing.T
	engine  *fakeLocalFlowEngine
	served  []gateway.ChatUpstream
	image   string
	ledger  string
	limits  error
	signer  *aepTestSigner
	failing bool
}

func newLFFixture(t *testing.T) *lfFixture {
	f := &lfFixture{
		t:      t,
		engine: &fakeLocalFlowEngine{imagePresent: true, stdout: `{"output":{"answer":1},"usage":{"input_tokens":0,"output_tokens":0}}` + "\n"},
		image:  lfTestImage,
		ledger: filepath.Join(t.TempDir(), "local-flow-runs"),
		signer: newAEPTestSigner(t),
	}
	prev := localFlowDepsFactory
	localFlowDepsFactory = f.deps
	t.Cleanup(func() { localFlowDepsFactory = prev })
	return f
}

func (f *lfFixture) deps() localFlowDeps {
	return localFlowDeps{
		image:           f.image,
		runtime:         func() catalog.ContainerRuntime { return catalog.ContainerRuntime{EngineBin: "docker", Bin: "docker"} },
		engines:         func(context.Context) []gateway.ChatUpstream { return f.served },
		preflightLimits: func(catalog.ContainerRuntime) error { return f.limits },
		stateDir:        func() (string, error) { return f.ledger, nil },
		exec:            f.engine.exec,
		newName:         func() (string, error) { return "citadel-local-flow-test", nil },
		now:             time.Now,
	}
}

type failingSignSigner struct{ *aepTestSigner }

func (failingSignSigner) Sign([]byte) ([]byte, error) { return nil, errors.New("sign unavailable") }

type keylessSigner struct{}

func (keylessSigner) Sign([]byte) ([]byte, error)           { return nil, errors.New("no key") }
func (keylessSigner) PublicKeyFingerprint() (string, error) { return "", errors.New("no key") }

func lfParams(t *testing.T, overrides map[string]string) json.RawMessage {
	t.Helper()
	p := map[string]string{
		"node_id":         lfTestNode,
		"organization_id": lfTestOrg,
		"flow_version_id": lfTestVersion,
		"run_id":          lfTestRunID,
		"package_digest":  lfDeterministicDigest,
		"graph_json":      lfDeterministicGraph,
		"input_json":      `{}`,
	}
	for k, v := range overrides {
		if v == "<delete>" {
			delete(p, k)
			continue
		}
		p[k] = v
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// lfJob builds the RUN_JOB_TEMPLATE job the platform would dispatch, on the
// node's own per-node stream.
func lfJob(t *testing.T, params json.RawMessage, queue string) *worker.Job {
	t.Helper()
	return &worker.Job{
		ID:          "job-test-1",
		Type:        worker.JobTypeRunJobTemplate,
		SourceQueue: queue,
		Payload: map[string]any{
			"template_key":     localFlowTemplateKey,
			"template_version": "1",
			"content_hash":     localFlowPinnedContentHash,
			"input_schema":     localFlowInputSchema,
			"output_schema":    localFlowOutputSchema,
			"runner":           localFlowRunner,
			"params":           string(params),
		},
	}
}

const lfOwnQueue = "jobs:v1:shell:org_" + lfTestOrg + ":node:" + lfTestNode

func (f *lfFixture) handler(ws string) *worker.RunJobTemplateHandler {
	var signer aep.Signer = f.signer
	if f.failing {
		signer = failingSignSigner{f.signer}
	}
	ops := liveTemplateRunOps{
		workspaceDir:  ws,
		nodeID:        lfTestNode,
		receiptSigner: signer,
		receiptNodeID: func() string { return "fabric-test-1" },
	}
	return worker.NewRunJobTemplateHandler(worker.RunJobTemplateHandlerConfig{Ops: ops, NodeID: lfTestNode})
}

func (f *lfFixture) execute(job *worker.Job) (*worker.JobResult, string) {
	f.t.Helper()
	ws := f.t.TempDir()
	res, err := f.handler(ws).Execute(context.Background(), job, nil)
	if err != nil {
		f.t.Fatalf("Execute returned a Go error: %v", err)
	}
	return res, ws
}

func TestLocalFlowPinnedManifestHash(t *testing.T) {
	// Pinned against Python compute_template_hash over the same manifest.
	const want = "sha256:439ce024f80e4a53dbbcca1ee053b78b320e502e433c4860620de791560a67ba"
	if localFlowPinnedContentHash != want {
		t.Fatalf("pinned content hash = %s, want %s", localFlowPinnedContentHash, want)
	}
	if _, ok := lookupBuiltinTemplateRunner(localFlowBuiltinName); !ok {
		t.Fatal("local-flow-run is not registered")
	}
	if entry, _ := lookupBuiltinTemplateRunner(localFlowBuiltinName); entry.attested == nil {
		t.Fatal("local-flow-run must be an attested builtin")
	}
	if err := jobs.ValidateTemplateParams(lfParams(t, nil), json.RawMessage(localFlowInputSchema), json.RawMessage(localFlowOutputSchema)); err != nil {
		t.Fatalf("closed input schema rejects a valid request: %v", err)
	}
	if err := jobs.ValidateTemplateParams(lfParams(t, map[string]string{"command": "sh"}), json.RawMessage(localFlowInputSchema), json.RawMessage(localFlowOutputSchema)); err == nil {
		t.Fatal("closed input schema accepted an extra command param")
	}
}

func TestLocalFlowExpiredClaimCannotEraseConcurrentNewClaim(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := filepath.Join(dir, lfTestRunID)
	if err := os.WriteFile(old, []byte("old job\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expired := now.Add(-localFlowRunRetention - time.Hour)
	if err := os.Chtimes(old, expired, expired); err != nil {
		t.Fatal(err)
	}
	// Both deliveries have already observed the expired entry. Expiry and the
	// claim must still be one operation across the two independent callers.
	for i := 0; i < 2; i++ {
		seen, err := localFlowRunSeen(dir, lfTestRunID, now, localFlowRunRetention)
		if err != nil || seen {
			t.Fatalf("stale preflight %d: seen=%v err=%v", i, seen, err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, job := range []string{"job-a", "job-b"} {
		go func(job string) {
			<-start
			results <- claimLocalFlowRun(dir, lfTestRunID, job, now, localFlowRunRetention)
		}(job)
	}
	close(start)
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, os.ErrExist)) ||
		(second == nil && errors.Is(first, os.ErrExist))) {
		t.Fatalf("claims were not exclusive: %v, %v", first, second)
	}
	claim, err := os.ReadFile(old)
	if err != nil || (string(claim) != "job-a\n" && string(claim) != "job-b\n") {
		t.Fatalf("new claim was not retained: %q %v", claim, err)
	}
}

func TestLocalFlowFailedContainerCleanupIsReconciledBeforeNextRun(t *testing.T) {
	f := newLFFixture(t)
	f.engine.exitCode = 2
	f.engine.removeErr = errors.New("engine cleanup unavailable")
	first, _ := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
	if first.Status != worker.JobStatusFailure || first.Error == nil || !strings.Contains(first.Error.Error(), "cleanup pending") {
		t.Fatalf("first run = %s %v, want cleanup pending", first.Status, first.Error)
	}
	marker := filepath.Join(f.ledger, ".cleanup-citadel-local-flow-test")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed cleanup lost its durable marker: %v", err)
	}
	f.engine.removeErr = nil
	f.engine.exitCode = 0
	secondID := "0f3c2b8e-5a41-4d6b-9c7e-1a2b3c4d5e70"
	second, _ := f.execute(lfJob(t, lfParams(t, map[string]string{"run_id": secondID}), lfOwnQueue))
	if second.Status != worker.JobStatusSuccess {
		t.Fatalf("second run = %s %v, want recovery", second.Status, second.Error)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup marker remains after recovery: %v", err)
	}
	if len(f.engine.removed) != 2 || f.engine.runCount() != 2 {
		t.Fatalf("cleanup attempts = %d, runs = %d, want 2 each", len(f.engine.removed), f.engine.runCount())
	}
}

func TestLocalFlowAlreadyRemovedContainerIsClean(t *testing.T) {
	f := newLFFixture(t)
	f.engine.exitCode = 2
	f.engine.removeErr = errors.New("no such container")
	f.engine.autoRemoveOnFailure = true
	res, _ := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
	if res.Status != worker.JobStatusFailure || res.Error == nil || !strings.Contains(res.Error.Error(), "engine failed") {
		t.Fatalf("result = %s %v, want original engine failure", res.Status, res.Error)
	}
	marker := filepath.Join(f.ledger, ".cleanup-citadel-local-flow-test")
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("already removed container left cleanup debt: %v", err)
	}
}

func TestLocalFlowCleanupRetriesAtWorkerStartupWithoutAnotherJob(t *testing.T) {
	f := newLFFixture(t)
	f.engine.exitCode = 2
	f.engine.removeErr = errors.New("engine cleanup unavailable")
	res, _ := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
	if res.Status != worker.JobStatusFailure || res.Error == nil || !strings.Contains(res.Error.Error(), "cleanup pending") {
		t.Fatalf("result = %s %v, want cleanup debt", res.Status, res.Error)
	}
	marker := filepath.Join(f.ledger, ".cleanup-citadel-local-flow-test")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("missing cleanup marker before restart: %v", err)
	}
	// A new worker with the same durable ledger recovers without a dispatched job.
	f.engine.removeErr = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	restartDeps := f.deps()
	restartDeps.runtime = func() catalog.ContainerRuntime { return catalog.ContainerRuntime{EngineBin: "podman", Bin: "podman"} }
	restartDeps.exec = func(cmd *exec.Cmd, stdin []byte, limit int) ([]byte, bool, error) {
		if len(cmd.Args) > 1 && cmd.Args[1] == "rm" && cmd.Args[0] != "docker" {
			return nil, false, fmt.Errorf("cleanup switched from recorded docker runtime to %s", cmd.Args[0])
		}
		return f.engine.exec(cmd, stdin, limit)
	}
	go func() {
		defer close(done)
		runLocalFlowCleanupReconciler(ctx, restartDeps, time.Hour, func(err error) { t.Errorf("startup cleanup: %v", err) })
	}()
	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("startup reconciler did not remove the cleanup debt")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if f.engine.runCount() != 1 {
		t.Fatalf("recovery launched a new run: %d", f.engine.runCount())
	}
}

func TestLocalFlowCleanupRetriesOnTimerWithoutAnotherJob(t *testing.T) {
	f := newLFFixture(t)
	f.engine.exitCode = 2
	f.engine.removeErr = errors.New("engine cleanup unavailable")
	res, _ := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
	if res.Status != worker.JobStatusFailure || res.Error == nil || !strings.Contains(res.Error.Error(), "cleanup pending") {
		t.Fatalf("result = %s %v, want cleanup debt", res.Status, res.Error)
	}
	marker := filepath.Join(f.ledger, ".cleanup-citadel-local-flow-test")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	failures := make(chan error, 1)
	go func() {
		defer close(done)
		runLocalFlowCleanupReconciler(ctx, f.deps(), 10*time.Millisecond, func(err error) {
			select {
			case failures <- err:
			default:
			}
		})
	}()
	select {
	case err := <-failures:
		if !strings.Contains(err.Error(), "engine cleanup unavailable") {
			t.Fatalf("first sweep error = %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("startup sweep did not attempt cleanup")
	}
	f.engine.mu.Lock()
	f.engine.removeErr = nil
	f.engine.mu.Unlock()
	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("periodic sweep did not clear cleanup debt")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if f.engine.runCount() != 1 {
		t.Fatalf("periodic recovery launched a new run: %d", f.engine.runCount())
	}
}

func TestLocalFlowPackageDigestMatchesPython(t *testing.T) {
	for _, tc := range []struct{ graph, want string }{{lfDeterministicGraph, lfDeterministicDigest}, {lfLLMGraph, lfLLMDigest}} {
		canonical, _, err := canonicalLocalFlowObject(tc.graph, localFlowMaxGraphBytes, "graph")
		if err != nil {
			t.Fatal(err)
		}
		if got := "sha256:" + sha256Hex(string(canonical)); got != tc.want {
			t.Fatalf("package digest = %s, want Python's %s", got, tc.want)
		}
	}
}

func TestLocalFlowDeterministicRunOnOwnedNode(t *testing.T) {
	f := newLFFixture(t)
	res, ws := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
	if res.Status != worker.JobStatusSuccess {
		t.Fatalf("status = %s, error = %v", res.Status, res.Error)
	}
	if f.engine.runCount() != 1 {
		t.Fatalf("engine runs = %d, want 1", f.engine.runCount())
	}
	args := f.engine.runs[0]
	assertLocalFlowHardenedArgs(t, args, "none")
	if !containsString(args, "CITADEL_LOCAL_MODELS=[]") || !containsString(args, fmt.Sprintf("CITADEL_LOCAL_GATEWAY_PORT=%d", localFlowUnusedGatewayPort)) {
		t.Fatalf("deterministic env wrong: %v", args)
	}
	var stdin map[string]any
	if err := json.Unmarshal(f.engine.stdins[0], &stdin); err != nil {
		t.Fatal(err)
	}
	if len(stdin) != 4 || stdin["run_id"] != lfTestRunID || stdin["package_digest"] != lfDeterministicDigest {
		t.Fatalf("engine request = %v", stdin)
	}

	out := res.Output
	outputs, _ := out["outputs"].([]any)
	if len(outputs) != 1 {
		t.Fatalf("outputs = %v", out["outputs"])
	}
	entry := outputs[0].(map[string]any)
	data, err := os.ReadFile(filepath.Join(ws, filepath.FromSlash(entry["path"].(string))))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"output":{"answer":1},"usage":{"input_tokens":0,"output_tokens":0}}`+"\n" {
		t.Fatalf("result.json = %q", data)
	}

	report := out["report"].(map[string]any)
	binding := report["binding"].(map[string]any)
	for key, want := range map[string]string{
		"node_id": lfTestNode, "organization_id": lfTestOrg, "job_id": "job-test-1", "run_id": lfTestRunID,
		"flow_version_id": lfTestVersion, "package_digest": lfDeterministicDigest, "engine_image": lfTestImage,
		"template_content_hash": localFlowPinnedContentHash, "network": "none",
		"input_sha256": "sha256:" + sha256Hex("{}"),
	} {
		if binding[key] != want {
			t.Errorf("binding[%s] = %v, want %s", key, binding[key], want)
		}
	}

	receipt := out["aep_receipt"].(map[string]any)
	if receipt["action"] != "run_job_template" || receipt["job_id"] != "job-test-1" || receipt["node_id"] != "fabric-test-1" {
		t.Fatalf("receipt = %v", receipt)
	}
	if receipt["input_sha256"] != report["binding_sha256"] {
		t.Fatalf("receipt input_sha256 %v does not bind the reported binding %v", receipt["input_sha256"], report["binding_sha256"])
	}
	wantOut := "sha256:" + sha256Hex(entry["sha256"].(string))
	if receipt["output_sha256"] != wantOut {
		t.Fatalf("receipt output_sha256 = %v, want %s", receipt["output_sha256"], wantOut)
	}
	assertLocalFlowReceiptVerifies(t, receipt, f.signer)
	if ws, _ := out["work_receipts"].([]any); len(ws) != 1 {
		t.Fatalf("work_receipts = %v", out["work_receipts"])
	}

	usage := out["usage"].(map[string]any)
	if usage["org_id"] != lfTestOrg || usage["origin"] != "dispatched" || usage["signed"] != true || usage["tokens_known"] != true {
		t.Fatalf("usage = %v", usage)
	}
}

func TestLocalFlowLocalLLMRunOnOwnedNode(t *testing.T) {
	f := newLFFixture(t)
	f.served = []gateway.ChatUpstream{
		{Engine: "ollama", Port: 11434, Models: []string{"other-model"}},
		{Engine: "vllm", Port: lfTestPort, Models: []string{lfTestModel}},
	}
	f.engine.stdout = `{"output":{"label":"x","response":"Local answer."},"usage":{"input_tokens":3,"output_tokens":2}}`
	res, _ := f.execute(lfJob(t, lfParams(t, map[string]string{
		"graph_json": lfLLMGraph, "package_digest": lfLLMDigest, "input_json": `{"prompt":"synthetic question"}`,
	}), lfOwnQueue))
	if res.Status != worker.JobStatusSuccess {
		t.Fatalf("status = %s, error = %v", res.Status, res.Error)
	}
	args := f.engine.runs[0]
	assertLocalFlowHardenedArgs(t, args, "host")
	if !containsString(args, fmt.Sprintf("CITADEL_LOCAL_GATEWAY_PORT=%d", lfTestPort)) || !containsString(args, `CITADEL_LOCAL_MODELS=["synthetic-model"]`) {
		t.Fatalf("LLM env wrong: %v", args)
	}
	usage := res.Output["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(3) || usage["completion_tokens"] != float64(2) || usage["total_tokens"] != float64(5) {
		t.Fatalf("usage = %v", usage)
	}
	assertLocalFlowReceiptVerifies(t, res.Output["aep_receipt"].(map[string]any), f.signer)
}

func TestLocalFlowUnknownTokenCountsStayUnknown(t *testing.T) {
	f := newLFFixture(t)
	f.served = []gateway.ChatUpstream{{Engine: "vllm", Port: lfTestPort, Models: []string{lfTestModel}}}
	f.engine.stdout = `{"output":{"label":"x","response":"r"},"usage":{"input_tokens":null,"output_tokens":null}}`
	res, _ := f.execute(lfJob(t, lfParams(t, map[string]string{"graph_json": lfLLMGraph, "package_digest": lfLLMDigest, "input_json": `{"prompt":"q"}`}), lfOwnQueue))
	if res.Status != worker.JobStatusSuccess {
		t.Fatalf("status = %s, error = %v", res.Status, res.Error)
	}
	usage := res.Output["usage"].(map[string]any)
	if usage["tokens_known"] != false || usage["input_tokens"] != nil {
		t.Fatalf("unknown tokens reported as known: %v", usage)
	}
	if _, ok := usage["prompt_tokens"]; ok {
		t.Fatalf("unknown prompt tokens reported as a number: %v", usage)
	}
}

// Every refusal below must happen before the engine container starts.
func TestLocalFlowRefusesBeforeExecution(t *testing.T) {
	changedGraph := strings.Replace(lfDeterministicGraph, `"value":1`, `"value":2`, 1)
	otherTemplateRunner := `{"kind":"builtin","handler":"local-flow-run"}`
	cases := []struct {
		name  string
		setup func(f *lfFixture)
		job   func(t *testing.T) *worker.Job
		want  string
	}{
		{name: "wrong node in request", job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, map[string]string{"node_id": "node-test-2"}), lfOwnQueue)
		}, want: "different node"},
		{name: "dispatched to another node's stream", job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, nil), "jobs:v1:shell:org_"+lfTestOrg+":node:node-test-2")
		}, want: "source queue does not match"},
		{name: "cross-organization request", job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, map[string]string{"organization_id": "org-test-2"}), lfOwnQueue)
		}, want: "different organization"},
		{name: "unapproved template naming the handler", job: func(t *testing.T) *worker.Job {
			job := lfJob(t, lfParams(t, nil), lfOwnQueue)
			// A self-consistent manifest (the worker's hash check passes) that is
			// not the pinned one.
			schema := strings.Replace(localFlowInputSchema, `"maxLength":524288`, `"maxLength":524289`, 1)
			hash, err := jobs.ComputeTemplateManifestHash(localFlowTemplateKey, 1, json.RawMessage(schema), json.RawMessage(localFlowOutputSchema), json.RawMessage(otherTemplateRunner))
			if err != nil {
				t.Fatal(err)
			}
			job.Payload["input_schema"] = schema
			job.Payload["content_hash"] = hash
			return job
		}, want: "only the pinned"},
		{name: "shell runner", job: func(t *testing.T) *worker.Job {
			job := lfJob(t, lfParams(t, nil), lfOwnQueue)
			runner := `{"kind":"shell","handler":"local-flow-run"}`
			hash, _ := jobs.ComputeTemplateManifestHash(localFlowTemplateKey, 1, json.RawMessage(localFlowInputSchema), json.RawMessage(localFlowOutputSchema), json.RawMessage(runner))
			job.Payload["runner"] = runner
			job.Payload["content_hash"] = hash
			return job
		}, want: "not permitted"},
		{name: "changed package", job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, map[string]string{"graph_json": changedGraph}), lfOwnQueue)
		}, want: "pinned digest"},
		{name: "malformed request", job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, map[string]string{"run_id": "not-a-uuid"}), lfOwnQueue)
		}, want: "input_schema"},
		{name: "graph with duplicate keys", job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, map[string]string{"graph_json": `{"edges":[],"edges":[]}`}), lfOwnQueue)
		}, want: "single JSON object"},
		{name: "unsupported node", job: func(t *testing.T) *worker.Job {
			g := strings.Replace(lfDeterministicGraph, `"ConstantInteger"`, `"APICall"`, 1)
			c, _, _ := canonicalLocalFlowObject(g, localFlowMaxGraphBytes, "graph")
			return lfJob(t, lfParams(t, map[string]string{"graph_json": g, "package_digest": "sha256:" + sha256Hex(string(c))}), lfOwnQueue)
		}, want: "unsupported node"},
		{name: "unsupported model", setup: func(f *lfFixture) {
			f.served = []gateway.ChatUpstream{{Engine: "vllm", Port: lfTestPort, Models: []string{"synthetic-model-large"}}}
		}, job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, map[string]string{"graph_json": lfLLMGraph, "package_digest": lfLLMDigest}), lfOwnQueue)
		}, want: "not served"},
		{name: "missing local LLM engine", job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, map[string]string{"graph_json": lfLLMGraph, "package_digest": lfLLMDigest}), lfOwnQueue)
		}, want: "not served"},
		{name: "no pinned engine image", setup: func(f *lfFixture) { f.image = "" }, job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, nil), lfOwnQueue)
		}, want: "no engine image is pinned"},
		{name: "engine image not installed", setup: func(f *lfFixture) { f.engine.imagePresent = false }, job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, nil), lfOwnQueue)
		}, want: "not present on this node"},
		{name: "limits not enforceable", setup: func(f *lfFixture) { f.limits = errors.New("pids not delegated") }, job: func(t *testing.T) *worker.Job {
			return lfJob(t, lfParams(t, nil), lfOwnQueue)
		}, want: "limits cannot be enforced"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLFFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			res, _ := f.execute(tc.job(t))
			if res.Status != worker.JobStatusFailure {
				t.Fatalf("status = %s, want failure", res.Status)
			}
			if res.Error == nil || !strings.Contains(res.Error.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", res.Error, tc.want)
			}
			if f.engine.runCount() != 0 {
				t.Fatalf("engine started %d time(s) for a refused run", f.engine.runCount())
			}
			if entries, _ := os.ReadDir(f.ledger); len(entries) != 0 {
				t.Fatalf("a refused run consumed its run id")
			}
		})
	}
}

func TestLocalFlowRefusesDuplicateRun(t *testing.T) {
	f := newLFFixture(t)
	if res, _ := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue)); res.Status != worker.JobStatusSuccess {
		t.Fatalf("first run failed: %v", res.Error)
	}
	res, _ := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
	if res.Status != worker.JobStatusFailure || !strings.Contains(res.Error.Error(), "duplicate run") {
		t.Fatalf("second run = %s %v, want duplicate refusal", res.Status, res.Error)
	}
	if f.engine.runCount() != 1 {
		t.Fatalf("engine runs = %d, want 1", f.engine.runCount())
	}
}

func TestLocalFlowReceiptFailureFailsClosed(t *testing.T) {
	t.Run("signer without a key refuses before execution", func(t *testing.T) {
		f := newLFFixture(t)
		ops := liveTemplateRunOps{workspaceDir: t.TempDir(), nodeID: lfTestNode, receiptSigner: keylessSigner{}, receiptNodeID: func() string { return "" }}
		h := worker.NewRunJobTemplateHandler(worker.RunJobTemplateHandlerConfig{Ops: ops, NodeID: lfTestNode})
		res, _ := h.Execute(context.Background(), lfJob(t, lfParams(t, nil), lfOwnQueue), nil)
		if res.Status != worker.JobStatusFailure || !errors.Is(res.Error, jobs.ErrReceiptSigningUnavailable) {
			t.Fatalf("result = %s %v, want receipt_signing_unavailable", res.Status, res.Error)
		}
		if f.engine.runCount() != 0 {
			t.Fatal("engine started without a usable signer")
		}
	})
	t.Run("sign failure after the run retains nothing", func(t *testing.T) {
		f := newLFFixture(t)
		f.failing = true
		res, ws := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
		if res.Status != worker.JobStatusFailure || !errors.Is(res.Error, jobs.ErrReceiptSigningUnavailable) {
			t.Fatalf("result = %s %v, want receipt_signing_unavailable", res.Status, res.Error)
		}
		if _, ok := res.Output["aep_receipt"]; ok {
			t.Fatal("unsigned result returned")
		}
		assertNoRetainedLocalFlowOutput(t, ws)
	})
}

func TestLocalFlowOutputFailure(t *testing.T) {
	cases := []struct {
		name, stdout, want string
		exit               int
	}{
		{name: "engine refusal category", stdout: `{"error":"duplicate run"}`, exit: 1, want: "refused the run: duplicate run"},
		{name: "unknown category is not echoed", stdout: `{"error":"synthetic-secret"}`, exit: 1, want: "refused the run: local execution failed"},
		{name: "malformed reply", stdout: `{"output":{},"usage":{"input_tokens":-1,"output_tokens":0}}`, want: "malformed result"},
		{name: "extra reply key", stdout: `{"output":{},"usage":{"input_tokens":0,"output_tokens":0},"cmd":"x"}`, want: "malformed result"},
		{name: "oversized reply", stdout: `{"output":{"x":"` + strings.Repeat("a", localFlowMaxStdoutBytes) + `"},"usage":{"input_tokens":0,"output_tokens":0}}`, want: "exceeds"},
		{name: "engine crash", stdout: "", exit: 137, want: "exit 137"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLFFixture(t)
			f.engine.stdout = tc.stdout
			f.engine.exitCode = tc.exit
			res, ws := f.execute(lfJob(t, lfParams(t, nil), lfOwnQueue))
			if res.Status != worker.JobStatusFailure || !strings.Contains(res.Error.Error(), tc.want) {
				t.Fatalf("result = %s %v, want %q", res.Status, res.Error, tc.want)
			}
			if strings.Contains(res.Error.Error(), "synthetic-secret") {
				t.Fatal("engine text echoed in the error")
			}
			assertNoRetainedLocalFlowOutput(t, ws)
		})
	}
}

func TestTemplateQueueOrgRequiresTheExactNodeStream(t *testing.T) {
	// Exercised through the handler: a pool queue never yields an org.
	f := newLFFixture(t)
	res, _ := f.execute(lfJob(t, lfParams(t, nil), "jobs:v1:shell:org_"+lfTestOrg))
	if res.Status != worker.JobStatusFailure {
		t.Fatalf("shared-pool delivery = %s, want failure", res.Status)
	}
	if f.engine.runCount() != 0 {
		t.Fatal("engine started for a shared-pool delivery")
	}
}

func assertLocalFlowHardenedArgs(t *testing.T, args []string, network string) {
	t.Helper()
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm -i", "--pull never", "--network " + network, "--cap-drop ALL",
		"--security-opt no-new-privileges", "--read-only", "--user " + localFlowContainerUser,
		"--cpus " + localFlowCPUs, "--memory " + localFlowMemory, "--pids-limit 256",
		"--mount " + localFlowRunsTmpfs,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run argv missing %q: %s", want, joined)
		}
	}
	for _, banned := range []string{" -v ", "--volume", "--privileged", "--entrypoint", "--cap-add"} {
		if strings.Contains(" "+joined+" ", banned) {
			t.Errorf("run argv contains %q: %s", banned, joined)
		}
	}
	if args[len(args)-1] != lfTestImage {
		t.Errorf("image is not the last argument, so a command could follow it: %v", args)
	}
	envs := 0
	for i, a := range args {
		if a == "-e" {
			envs++
			if !strings.HasPrefix(args[i+1], "CITADEL_LOCAL_") {
				t.Errorf("unexpected container env %q", args[i+1])
			}
		}
	}
	if envs != 3 {
		t.Errorf("container env count = %d, want 3", envs)
	}
}

func assertNoRetainedLocalFlowOutput(t *testing.T, ws string) {
	t.Helper()
	var found []string
	_ = filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("failed run retained files: %v", found)
	}
}

func assertLocalFlowReceiptVerifies(t *testing.T, receipt map[string]any, signer *aepTestSigner) {
	t.Helper()
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	outcome := verifyAEPReceipt(verifyOptions{
		receiptPath: path,
		nodeKeyFn:   func() (*ecdsa.PublicKey, error) { return &signer.key.PublicKey, nil },
	})
	if !outcome.Valid {
		t.Fatalf("receipt does not verify: %s", outcome.Reason)
	}
}

// TestLocalFlowShippedArtifact runs the builtin against a real, locally
// present aceteam#13130 image. It is opt-in: it needs a container engine and
// the image already pulled (the test never pulls).
//
//	CITADEL_LOCAL_FLOW_ARTIFACT_IMAGE=<repository>@sha256:<digest> go test ./cmd -run TestLocalFlowShippedArtifact
func TestLocalFlowShippedArtifact(t *testing.T) {
	image := os.Getenv("CITADEL_LOCAL_FLOW_ARTIFACT_IMAGE")
	if image == "" {
		t.Skip("set CITADEL_LOCAL_FLOW_ARTIFACT_IMAGE to a locally present, digest-pinned local_flow image")
	}
	deps := liveLocalFlowDeps()
	deps.image = image
	deps.engines = func(context.Context) []gateway.ChatUpstream { return nil }
	deps.stateDir = func() (string, error) { return filepath.Join(t.TempDir(), "runs"), nil }
	outDir := t.TempDir()
	bind := templateRunBinding{
		JobID: "job-artifact-1", NodeID: lfTestNode, OrgID: lfTestOrg,
		TemplateKey: localFlowTemplateKey, TemplateVersion: localFlowTemplateVersion, ContentHash: localFlowPinnedContentHash,
	}
	res, err := runLocalFlowBuiltin(context.Background(), bind, lfParams(t, nil), nil, outDir, deps)
	if err != nil {
		t.Fatalf("shipped engine run failed: %v", err)
	}
	output := res.Report["output"].(map[string]any)
	if fmt.Sprint(output["answer"]) != "1" {
		t.Fatalf("output = %v, want answer 1", output)
	}
}

func TestLocalFlowRefusesAStreamWithoutAnOrganization(t *testing.T) {
	// Passes the worker's per-node gate and node suffix, but is not the
	// organization's shell stream, so no organization can be bound.
	f := newLFFixture(t)
	res, _ := f.execute(lfJob(t, lfParams(t, nil), "jobs:v1:tag:org_"+lfTestOrg+":node:"+lfTestNode))
	if res.Status != worker.JobStatusFailure || !strings.Contains(res.Error.Error(), "did not arrive on this node's organization stream") {
		t.Fatalf("result = %s %v, want organization-stream refusal", res.Status, res.Error)
	}
	if f.engine.runCount() != 0 {
		t.Fatal("engine started without a bound organization")
	}
}

func TestHPCProfileRefusesLocalFlowRun(t *testing.T) {
	// local-flow-run may use host networking, which a shared compute node must
	// never grant.
	if hpcTemplateBuiltinPolicy(localFlowBuiltinName) == "" {
		t.Fatal("the hpc profile allows local-flow-run")
	}
}
