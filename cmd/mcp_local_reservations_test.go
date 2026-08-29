// cmd/mcp_local_reservations_test.go
//
// Hermetic tests for local_model_deploy/local_run_exclusive/local_model_stop
// (aceteam#8248/#8249 v2). Every side-effecting dependency (moduleControl,
// chatLister, reservations.*) is stubbed -- these tests never construct a
// real internal/jobs.ServiceHandler, read a real manifest, or touch
// docker/GPU. See docs/design-model-exclusivity.md.
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
)

// ============================================================================
// local_model_deploy
// ============================================================================

func TestLocalModelDeployCallRequiresModelAndEngine(t *testing.T) {
	deps := localMCPDeps{reservations: localReservationOps{
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			t.Fatalf("deploy should not be called without both model and engine")
			return "", nil
		},
	}}
	tools := newModelExclusivityTools(deps)
	deploy, ok := findLocalTool(tools, "local_model_deploy")
	if !ok {
		t.Fatal("local_model_deploy not registered")
	}

	if _, err := deploy.Call(context.Background(), json.RawMessage(`{"engine":"vllm"}`)); err == nil {
		t.Error("expected error for missing model")
	}
	if _, err := deploy.Call(context.Background(), json.RawMessage(`{"model":"foo"}`)); err == nil {
		t.Error("expected error for missing engine")
	}
}

func TestLocalModelDeployCallForwardsArgsAndVRAM(t *testing.T) {
	type call struct {
		jobID, serviceName, model string
		requiredVRAMBytes         uint64
	}
	var got call
	deps := localMCPDeps{reservations: localReservationOps{
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			got = call{jobID, serviceName, model, requiredVRAMBytes}
			return `{"ok":true}`, nil
		},
	}}
	tools := newModelExclusivityTools(deps)
	deploy, _ := findLocalTool(tools, "local_model_deploy")

	out, err := deploy.Call(context.Background(), json.RawMessage(`{"model":"prism-ml/Bonsai-27B-gguf","engine":"bonsai","vram_mb":4096}`))
	if err != nil {
		t.Fatalf("deploy.Call: %v", err)
	}
	if out != `{"ok":true}` {
		t.Errorf("out = %q", out)
	}
	if got.serviceName != "bonsai" || got.model != "prism-ml/Bonsai-27B-gguf" {
		t.Errorf("call = %+v, want engine=bonsai model=prism-ml/Bonsai-27B-gguf", got)
	}
	if got.requiredVRAMBytes != 4096*1024*1024 {
		t.Errorf("requiredVRAMBytes = %d, want %d (4096 MB)", got.requiredVRAMBytes, 4096*1024*1024)
	}
}

func TestLocalModelDeployCallNoVRAMMeansZeroBudget(t *testing.T) {
	var gotVRAM uint64 = 999 // sentinel to prove it gets overwritten to 0
	deps := localMCPDeps{reservations: localReservationOps{
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			gotVRAM = requiredVRAMBytes
			return "", nil
		},
	}}
	tools := newModelExclusivityTools(deps)
	deploy, _ := findLocalTool(tools, "local_model_deploy")
	if _, err := deploy.Call(context.Background(), json.RawMessage(`{"model":"m","engine":"vllm"}`)); err != nil {
		t.Fatalf("deploy.Call: %v", err)
	}
	if gotVRAM != 0 {
		t.Errorf("requiredVRAMBytes = %d, want 0 (no vram_mb declared)", gotVRAM)
	}
}

func TestLocalModelDeployCallPropagatesError(t *testing.T) {
	deps := localMCPDeps{reservations: localReservationOps{
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			return "", errors.New("pull failed: disk full")
		},
	}}
	tools := newModelExclusivityTools(deps)
	deploy, _ := findLocalTool(tools, "local_model_deploy")
	_, err := deploy.Call(context.Background(), json.RawMessage(`{"model":"m","engine":"vllm"}`))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("expected propagated error, got %v", err)
	}
}

// ============================================================================
// local_run_exclusive
// ============================================================================

func TestLocalRunExclusiveDefaultEvictsEverythingThenStarts(t *testing.T) {
	var reserveExcCalls []string
	var deployCalls []string
	deps := localMCPDeps{reservations: localReservationOps{
		reserveExclusive: func(jobID, exclude string) ([]string, string, error) {
			reserveExcCalls = append(reserveExcCalls, jobID+"/"+exclude)
			return []string{"vllm", "ollama"}, "evicted 2", nil
		},
		reserveBudget: func(jobID string, requiredVRAMBytes uint64) ([]string, string, error) {
			t.Fatal("reserveBudget should not be called when vram_mb is absent")
			return nil, "", nil
		},
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			deployCalls = append(deployCalls, jobID+"/"+serviceName+"/"+model)
			if requiredVRAMBytes != 0 {
				t.Errorf("deploy called with requiredVRAMBytes=%d, want 0 (peers already evicted)", requiredVRAMBytes)
			}
			return `{"running":true}`, nil
		},
	}}
	tools := newModelExclusivityTools(deps)
	run, ok := findLocalTool(tools, "local_run_exclusive")
	if !ok {
		t.Fatal("local_run_exclusive not registered")
	}

	out, err := run.Call(context.Background(), json.RawMessage(`{"model":"Bonsai-27B-Q1_0.gguf","engine":"bonsai"}`))
	if err != nil {
		t.Fatalf("run.Call: %v", err)
	}
	if !equalStringsCmd(reserveExcCalls, []string{"exclusive:bonsai/bonsai"}) {
		t.Errorf("reserveExclusive calls = %v", reserveExcCalls)
	}
	if !equalStringsCmd(deployCalls, []string{"exclusive:bonsai/bonsai/Bonsai-27B-Q1_0.gguf"}) {
		t.Errorf("deploy calls = %v", deployCalls)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v (%s)", err, out)
	}
	if parsed["reservation_id"] != "exclusive:bonsai" {
		t.Errorf("reservation_id = %v, want exclusive:bonsai", parsed["reservation_id"])
	}
	evicted, _ := parsed["evicted"].([]any)
	if len(evicted) != 2 {
		t.Errorf("evicted = %v, want 2 entries (the blast radius must always be returned)", parsed["evicted"])
	}
}

func TestLocalRunExclusiveWithVRAMUsesBoundedReserve(t *testing.T) {
	var usedExclusive, usedBudget bool
	var gotBudget uint64
	deps := localMCPDeps{reservations: localReservationOps{
		reserveExclusive: func(jobID, exclude string) ([]string, string, error) {
			usedExclusive = true
			return nil, "", nil
		},
		reserveBudget: func(jobID string, requiredVRAMBytes uint64) ([]string, string, error) {
			usedBudget = true
			gotBudget = requiredVRAMBytes
			return []string{"ollama"}, "reserved 8GB", nil
		},
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			return `{}`, nil
		},
	}}
	tools := newModelExclusivityTools(deps)
	run, _ := findLocalTool(tools, "local_run_exclusive")

	if _, err := run.Call(context.Background(), json.RawMessage(`{"model":"m","engine":"vllm","vram_mb":8192}`)); err != nil {
		t.Fatalf("run.Call: %v", err)
	}
	if usedExclusive {
		t.Error("reserveExclusive was called despite vram_mb being set")
	}
	if !usedBudget {
		t.Error("reserveBudget was not called despite vram_mb being set")
	}
	if gotBudget != 8192*1024*1024 {
		t.Errorf("budget = %d, want %d (8192 MB)", gotBudget, uint64(8192)*1024*1024)
	}
}

func TestLocalRunExclusiveReserveFailurePropagatesWithoutDeploying(t *testing.T) {
	deployed := false
	deps := localMCPDeps{reservations: localReservationOps{
		reserveExclusive: func(jobID, exclude string) ([]string, string, error) {
			return nil, "", errors.New("could not collect node status")
		},
		reserveBudget: func(jobID string, requiredVRAMBytes uint64) ([]string, string, error) {
			return nil, "", nil
		},
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			deployed = true
			return "", nil
		},
	}}
	tools := newModelExclusivityTools(deps)
	run, _ := findLocalTool(tools, "local_run_exclusive")

	_, err := run.Call(context.Background(), json.RawMessage(`{"model":"m","engine":"vllm"}`))
	if err == nil || !strings.Contains(err.Error(), "could not collect node status") {
		t.Fatalf("expected reserve error to propagate, got %v", err)
	}
	if deployed {
		t.Error("deploy was called despite the reservation failing")
	}
}

func TestLocalRunExclusiveDeployFailureReportsReservationStillHeld(t *testing.T) {
	deps := localMCPDeps{reservations: localReservationOps{
		reserveExclusive: func(jobID, exclude string) ([]string, string, error) {
			return []string{"ollama"}, "evicted 1", nil
		},
		deploy: func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) {
			return "", errors.New("docker compose up failed")
		},
	}}
	tools := newModelExclusivityTools(deps)
	run, _ := findLocalTool(tools, "local_run_exclusive")

	_, err := run.Call(context.Background(), json.RawMessage(`{"model":"m","engine":"bonsai"}`))
	if err == nil {
		t.Fatal("expected an error when the start-half fails")
	}
	for _, want := range []string{"docker compose up failed", "HELD", "ollama", "local_model_stop", "exclusive:bonsai"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q (operator needs to know the reservation is still held and how to release it)", err.Error(), want)
		}
	}
}

func TestLocalRunExclusiveRequiresModelAndEngine(t *testing.T) {
	deps := localMCPDeps{reservations: localReservationOps{
		reserveExclusive: func(jobID, exclude string) ([]string, string, error) {
			t.Fatal("should not reserve without both model and engine")
			return nil, "", nil
		},
		reserveBudget: func(jobID string, requiredVRAMBytes uint64) ([]string, string, error) { return nil, "", nil },
		deploy:        func(jobID, serviceName, model string, requiredVRAMBytes uint64) (string, error) { return "", nil },
	}}
	tools := newModelExclusivityTools(deps)
	run, _ := findLocalTool(tools, "local_run_exclusive")

	if _, err := run.Call(context.Background(), json.RawMessage(`{"engine":"vllm"}`)); err == nil {
		t.Error("expected error for missing model")
	}
	if _, err := run.Call(context.Background(), json.RawMessage(`{"model":"m"}`)); err == nil {
		t.Error("expected error for missing engine")
	}
}

// ============================================================================
// local_model_stop
// ============================================================================

func stubChatLister(models map[string]string) gateway.ChatModelLister {
	// models: modelID -> engine name. Builds one ChatUpstream per engine, each
	// carrying every model routed to it.
	byEngine := map[string][]string{}
	for model, engine := range models {
		byEngine[engine] = append(byEngine[engine], model)
	}
	return func() []gateway.ChatUpstream {
		out := make([]gateway.ChatUpstream, 0, len(byEngine))
		for engine, mods := range byEngine {
			out = append(out, gateway.ChatUpstream{Engine: engine, Port: 8000, Models: mods})
		}
		return out
	}
}

func TestLocalModelStopResolvesModelToEngineAndStopsIt(t *testing.T) {
	var stoppedName string
	var stoppedAction moduleAction
	deps := localMCPDeps{
		chatLister: stubChatLister(map[string]string{"Bonsai-27B-Q1_0.gguf": "bonsai"}),
		moduleControl: func(name string, action moduleAction) (string, error) {
			stoppedName, stoppedAction = name, action
			return "stopped", nil
		},
	}
	tools := newModelExclusivityTools(deps)
	stop, ok := findLocalTool(tools, "local_model_stop")
	if !ok {
		t.Fatal("local_model_stop not registered")
	}

	out, err := stop.Call(context.Background(), json.RawMessage(`{"model":"Bonsai-27B-Q1_0.gguf"}`))
	if err != nil {
		t.Fatalf("stop.Call: %v", err)
	}
	if stoppedName != "bonsai" || stoppedAction != moduleActionStop {
		t.Errorf("moduleControl called with (%q, %v), want (bonsai, stop)", stoppedName, stoppedAction)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v (%s)", err, out)
	}
	if parsed["engine"] != "bonsai" {
		t.Errorf("engine = %v, want bonsai", parsed["engine"])
	}
	if _, hasReservation := parsed["reservation_id"]; hasReservation {
		t.Error("reservation_id present despite hasActiveReservation not being wired/true")
	}
}

func TestLocalModelStopUnknownModelErrors(t *testing.T) {
	deps := localMCPDeps{
		chatLister: stubChatLister(map[string]string{"other-model": "vllm"}),
		moduleControl: func(name string, action moduleAction) (string, error) {
			t.Fatal("moduleControl should not be called for an unresolvable model")
			return "", nil
		},
	}
	tools := newModelExclusivityTools(deps)
	stop, _ := findLocalTool(tools, "local_model_stop")
	if _, err := stop.Call(context.Background(), json.RawMessage(`{"model":"does-not-exist"}`)); err == nil {
		t.Fatal("expected an error for a model not served locally")
	}
}

// TestLocalModelStopOrderingStopsBeforeRelease pins the advisor-caught
// ordering fix: the target is stopped FIRST, THEN the reservation (if any)
// is released -- releasing first would restart evicted peers while the
// target still holds its own VRAM.
func TestLocalModelStopOrderingStopsBeforeRelease(t *testing.T) {
	var order []string
	deps := localMCPDeps{
		chatLister: stubChatLister(map[string]string{"bonsai-model": "bonsai"}),
		moduleControl: func(name string, action moduleAction) (string, error) {
			order = append(order, "stop:"+name)
			return "stopped", nil
		},
		reservations: localReservationOps{
			hasActiveReservation: func(jobID string) (bool, error) {
				order = append(order, "hasActive:"+jobID)
				return true, nil
			},
			release: func(jobID string) ([]string, error) {
				order = append(order, "release:"+jobID)
				return []string{"vllm", "ollama"}, nil
			},
		},
	}
	tools := newModelExclusivityTools(deps)
	stop, _ := findLocalTool(tools, "local_model_stop")

	out, err := stop.Call(context.Background(), json.RawMessage(`{"model":"bonsai-model"}`))
	if err != nil {
		t.Fatalf("stop.Call: %v", err)
	}
	want := []string{"stop:bonsai", "hasActive:exclusive:bonsai", "release:exclusive:bonsai"}
	if !equalStringsCmd(order, want) {
		t.Fatalf("call order = %v, want %v (stop MUST precede release)", order, want)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("result not valid JSON: %v (%s)", err, out)
	}
	if parsed["reservation_id"] != "exclusive:bonsai" {
		t.Errorf("reservation_id = %v, want exclusive:bonsai", parsed["reservation_id"])
	}
	restored, _ := parsed["restored"].([]any)
	if len(restored) != 2 {
		t.Errorf("restored = %v, want 2 entries", parsed["restored"])
	}
}

func TestLocalModelStopNoActiveReservationSkipsRelease(t *testing.T) {
	releaseCalled := false
	deps := localMCPDeps{
		chatLister: stubChatLister(map[string]string{"m": "vllm"}),
		moduleControl: func(name string, action moduleAction) (string, error) {
			return "stopped", nil
		},
		reservations: localReservationOps{
			hasActiveReservation: func(jobID string) (bool, error) { return false, nil },
			release: func(jobID string) ([]string, error) {
				releaseCalled = true
				return nil, nil
			},
		},
	}
	tools := newModelExclusivityTools(deps)
	stop, _ := findLocalTool(tools, "local_model_stop")
	out, err := stop.Call(context.Background(), json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("stop.Call: %v", err)
	}
	if releaseCalled {
		t.Error("release was called despite hasActiveReservation reporting false")
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(out), &parsed)
	if _, present := parsed["reservation_id"]; present {
		t.Error("reservation_id present despite no active reservation")
	}
}

func TestLocalModelStopStopFailurePreventsRelease(t *testing.T) {
	releaseCalled := false
	deps := localMCPDeps{
		chatLister: stubChatLister(map[string]string{"m": "vllm"}),
		moduleControl: func(name string, action moduleAction) (string, error) {
			return "", errors.New("compose down failed")
		},
		reservations: localReservationOps{
			hasActiveReservation: func(jobID string) (bool, error) { return true, nil },
			release: func(jobID string) ([]string, error) {
				releaseCalled = true
				return nil, nil
			},
		},
	}
	tools := newModelExclusivityTools(deps)
	stop, _ := findLocalTool(tools, "local_model_stop")
	_, err := stop.Call(context.Background(), json.RawMessage(`{"model":"m"}`))
	if err == nil || !strings.Contains(err.Error(), "compose down failed") {
		t.Fatalf("expected stop error to propagate, got %v", err)
	}
	if releaseCalled {
		t.Error("release was called despite the stop failing -- the reservation must stay held for a retry")
	}
}

func TestLocalModelStopRequiresModel(t *testing.T) {
	deps := localMCPDeps{chatLister: stubChatLister(nil)}
	tools := newModelExclusivityTools(deps)
	stop, _ := findLocalTool(tools, "local_model_stop")
	if _, err := stop.Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("expected error for missing model")
	}
}

// equalStringsCmd is a package-scoped rename to avoid colliding with any
// identically-named helper elsewhere in the cmd test package.
func equalStringsCmd(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
