package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/status"
)

// serviceHandlerWithAdoptSeams returns a ServiceHandler wired with the #1081
// adoption seams so the decision is exercised without a container runtime or a
// live engine. ourContainerRunning is fed through dockerServiceRunningFn (which
// makes ownership "determinable"); serving/models come from externalEngineServingFn.
func serviceHandlerWithAdoptSeams(t *testing.T, ourContainerRunning, serving bool, models []string) *ServiceHandler {
	t.Helper()
	h := NewServiceHandler(t.TempDir())
	h.dockerServiceRunningFn = func(string) bool { return ourContainerRunning }
	h.externalEngineServingFn = func(_ context.Context, _ string, _ int) ([]string, bool) {
		return models, serving
	}
	return h
}

// TestMaybeAdoptExternalEngine_Decision pins the aceteam-ai/citadel-cli#1081
// adoption decision across its branches, all hermetic via the seams.
func TestMaybeAdoptExternalEngine_Decision(t *testing.T) {
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}

	t.Run("adopts when external serving and our container not running", func(t *testing.T) {
		h := serviceHandlerWithAdoptSeams(t, false, true, []string{"Qwen/Qwen3-8B"})
		out, adopted := h.maybeAdoptExternalEngine(testCtx(), svc, "", "docker")
		if !adopted {
			t.Fatal("expected adopted=true")
		}
		var res serviceResult
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !res.Running || res.Kind != "docker" || res.Action != "start" {
			t.Fatalf("unexpected result: %+v", res)
		}
		if !strings.Contains(res.Message, "adopted external vllm") || !strings.Contains(res.Message, "Qwen/Qwen3-8B") {
			t.Errorf("message = %q, want it to mention adoption + the served model", res.Message)
		}
		wantEndpoint := fmt.Sprintf("127.0.0.1:%d", mustAdoptPort(t, "vllm"))
		if res.Endpoint != wantEndpoint {
			t.Errorf("endpoint = %q, want %q", res.Endpoint, wantEndpoint)
		}
	})

	t.Run("does not adopt when nothing external is serving", func(t *testing.T) {
		h := serviceHandlerWithAdoptSeams(t, false, false, nil)
		if _, adopted := h.maybeAdoptExternalEngine(testCtx(), svc, "", "docker"); adopted {
			t.Fatal("expected adopted=false when no external engine is serving")
		}
	})

	t.Run("does not adopt when our own container is running", func(t *testing.T) {
		// Our managed container answering /v1/models is NOT adoption; the ordinary
		// already-running path owns that case.
		h := serviceHandlerWithAdoptSeams(t, true, true, []string{"Qwen/Qwen3-8B"})
		if _, adopted := h.maybeAdoptExternalEngine(testCtx(), svc, "", "docker"); adopted {
			t.Fatal("expected adopted=false when citadel's own container is running")
		}
	})

	t.Run("does not adopt a non-allowlisted engine", func(t *testing.T) {
		h := serviceHandlerWithAdoptSeams(t, false, true, []string{"llama3"})
		ollama := manifestService{Name: "ollama", Type: "docker", ComposeFile: "services/ollama.yml"}
		if _, adopted := h.maybeAdoptExternalEngine(testCtx(), ollama, "", "docker"); adopted {
			t.Fatal("expected adopted=false for a non-allowlisted engine")
		}
	})

	t.Run("model mismatch is logged but still adopts", func(t *testing.T) {
		var logs []string
		ctx := JobContext{LogFn: func(_, msg string) { logs = append(logs, msg) }}
		h := serviceHandlerWithAdoptSeams(t, false, true, []string{"Qwen/Other"})
		_, adopted := h.maybeAdoptExternalEngine(ctx, svc, "Qwen/Requested", "docker")
		if !adopted {
			t.Fatal("expected adopted=true even on model mismatch (launching would collide)")
		}
		joined := strings.Join(logs, "\n")
		if !strings.Contains(joined, "not served by the external") {
			t.Errorf("expected a mismatch warning, logs:\n%s", joined)
		}
	})

	t.Run("docker-less box (no runtime CLI) with external serving ADOPTS (#1084)", func(t *testing.T) {
		// No dockerServiceRunningFn seam -> ourContainerRunning does the real
		// exec.LookPath, which fails with PATH neutered, so ownership is
		// undeterminable -- the docker-less RM-01 shape. Post-#1084 a box with no
		// container runtime cannot be running our container, so a serving external
		// engine there is adopted rather than declined. The external probe is
		// seamed so no real 127.0.0.1:<port> request fires.
		t.Setenv("PATH", t.TempDir())
		h := NewServiceHandler(t.TempDir())
		h.externalEngineServingFn = func(_ context.Context, _ string, _ int) ([]string, bool) {
			return []string{"Qwen/Qwen3-8B"}, true
		}
		out, adopted := h.maybeAdoptExternalEngine(testCtx(), svc, "", "docker")
		if !adopted {
			t.Fatal("expected adopted=true on a docker-less box with an external engine serving (#1084)")
		}
		var res serviceResult
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !res.Running || !strings.Contains(res.Message, "adopted external vllm") {
			t.Fatalf("unexpected result: %+v", res)
		}
	})

	t.Run("docker-less box (no runtime CLI) with nothing serving does not adopt", func(t *testing.T) {
		// The other #1084 branch: a runtime-less box with no external engine has
		// nothing to adopt and must fall through (serviceStart then fails loudly at
		// the compose-up preflight -- there is nothing that can be launched either).
		t.Setenv("PATH", t.TempDir())
		h := NewServiceHandler(t.TempDir())
		h.externalEngineServingFn = func(_ context.Context, _ string, _ int) ([]string, bool) {
			return nil, false
		}
		if _, adopted := h.maybeAdoptExternalEngine(testCtx(), svc, "", "docker"); adopted {
			t.Fatal("expected adopted=false on a docker-less box with nothing serving")
		}
	})
}

// TestServiceStart_AdoptsExternalOnDockerlessBox is the aceteam-ai/citadel-cli#1084
// acceptance: a full serviceStart on a box with NO container runtime (PATH
// neutered) and an external vLLM serving must ADOPT -- return success, never
// attempt a docker launch. The manifestService has NO compose_file, so any
// fall-through to the launch path would error at resolveComposePath; the clean
// adopted return is the proof no compose action was taken on a runtime-less box.
func TestServiceStart_AdoptsExternalOnDockerlessBox(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // docker-less box: no runtime CLI reachable
	h := NewServiceHandler(t.TempDir())
	h.externalEngineServingFn = func(_ context.Context, _ string, _ int) ([]string, bool) {
		return []string{"Qwen/Qwen3-8B"}, true
	}
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: ""}

	out, err := h.serviceStart(testCtx(), svc, "", 0, 0, trustRemoteCodeUnspecified)
	if err != nil {
		t.Fatalf("serviceStart returned error (attempted a launch on a docker-less box?): %v", err)
	}
	var res serviceResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.Running || !strings.Contains(res.Message, "adopted external vllm") {
		t.Fatalf("expected an adopted result on a docker-less box, got %+v", res)
	}
	if res.Error != "" {
		t.Fatalf("adopted result must carry no error, got %q", res.Error)
	}
}

// TestServiceStart_NoExternalNoDockerErrorsClearly is the #1084 acceptance #2: a
// box with NO container runtime and NO external engine has nothing to adopt and
// nothing to launch, so serviceStart must fail loudly with the docker-missing
// diagnosis (never a silent success). Driven through Execute with a materialized
// compose file so it reaches the compose-up preflight.
func TestServiceStart_NoExternalNoDockerErrorsClearly(t *testing.T) {
	t.Setenv("PATH", t.TempDir())  // docker-less box
	h, _ := newModelTestHandler(t) // seams externalEngineServingFn -> nothing serving

	out, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "job-noadopt-nodocker",
		Type:    "SERVICE_START",
		Payload: map[string]string{"service": "vllm"},
	})
	if err != nil {
		t.Fatalf("Execute SERVICE_START: %v", err)
	}
	if !strings.Contains(string(out), "docker compose up failed") ||
		!strings.Contains(string(out), "docker CLI not found on PATH") {
		t.Fatalf("expected the docker-missing failure (nothing to adopt, nothing to launch), got: %s", out)
	}
	if strings.Contains(string(out), "adopted external") {
		t.Fatalf("must not adopt when nothing external is serving, got: %s", out)
	}
}

// TestServiceStatus_AdoptedExternalReportsServing pins the #1084 SERVICE_STATUS
// half: an adopted external engine (our container not running, external serving)
// is reported RUNNING and externally-managed, sourced from the /v1/models probe
// rather than a citadel container that does not exist.
func TestServiceStatus_AdoptedExternalReportsServing(t *testing.T) {
	// dockerServiceRunningFn=false -> ourContainerRunning determinable && !running,
	// so the external probe decides; externalEngineServingFn reports serving.
	h := serviceHandlerWithAdoptSeams(t, false, true, []string{"Qwen/Qwen3-8B"})
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}

	out, err := h.serviceStatus(context.Background(), svc)
	if err != nil {
		t.Fatalf("serviceStatus: %v", err)
	}
	var res serviceResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.Running {
		t.Fatalf("expected an adopted external engine to report running, got %+v", res)
	}
	if !strings.Contains(res.Message, "externally managed") || !strings.Contains(res.Message, "Qwen/Qwen3-8B") {
		t.Errorf("message = %q, want externally-managed + the served model", res.Message)
	}
	wantEndpoint := fmt.Sprintf("127.0.0.1:%d", mustAdoptPort(t, "vllm"))
	if res.Endpoint != wantEndpoint {
		t.Errorf("endpoint = %q, want %q", res.Endpoint, wantEndpoint)
	}
}

// TestServiceStop_AdoptedExternalIsNoOp pins the #1084 SERVICE_STOP half: an
// adopted external engine is externally managed, so STOP is a clear no-op that
// leaves it running and does NOT consult/compose-down a citadel container.
func TestServiceStop_AdoptedExternalIsNoOp(t *testing.T) {
	h := serviceHandlerWithAdoptSeams(t, false, true, []string{"Qwen/Qwen3-8B"})
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: "services/vllm.yml"}

	out, err := h.serviceStop(testCtx(), svc)
	if err != nil {
		t.Fatalf("serviceStop: %v", err)
	}
	var res serviceResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.Running {
		t.Errorf("adopted external engine is not stopped, expected Running=true, got %+v", res)
	}
	if !strings.Contains(res.Message, "externally managed") || !strings.Contains(res.Message, "not stopping") {
		t.Errorf("message = %q, want an externally-managed no-op", res.Message)
	}
	if res.Error != "" {
		t.Errorf("no-op stop must carry no error, got %q", res.Error)
	}
}

// TestServiceStart_AdoptShortCircuitsBeforeLaunch drives the full serviceStart
// docker branch and pins the WIRING: with adoption true, serviceStart returns
// the adopted result and NEVER reaches compose resolution/launch or persists a
// model. The manifestService has NO compose_file, so any fall-through to the
// launch path would error at resolveComposePath -- the clean adopted return is
// the proof no compose-up was attempted.
func TestServiceStart_AdoptShortCircuitsBeforeLaunch(t *testing.T) {
	h := serviceHandlerWithAdoptSeams(t, false, true, []string{"Qwen/Other"})
	// A requested model that the external engine does not serve: adoption must
	// still win, and must NOT persist a <name>.env (that would be a fall-through
	// to the model-persistence step below adoption).
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: ""}

	out, err := h.serviceStart(testCtx(), svc, "Qwen/Requested", 0, 0, trustRemoteCodeUnspecified)
	if err != nil {
		t.Fatalf("serviceStart returned error (fell through to launch?): %v", err)
	}
	var res serviceResult
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.Running || !strings.Contains(res.Message, "adopted external vllm") {
		t.Fatalf("expected an adopted result, got %+v", res)
	}
}

// TestServiceStart_NoAdoptFallsThroughToLaunch is the negative wiring: with the
// adoption probe reporting nothing serving, serviceStart proceeds PAST the
// adoption check toward launch. With no compose_file, that surfaces as
// resolveComposePath's error -- proof the adoption branch was skipped and the
// launch path was taken -- and, critically, without shelling any container
// runtime (both docker probes are seamed).
func TestServiceStart_NoAdoptFallsThroughToLaunch(t *testing.T) {
	h := serviceHandlerWithAdoptSeams(t, false, false, nil) // nothing serving
	svc := manifestService{Name: "vllm", Type: "docker", ComposeFile: ""}

	_, err := h.serviceStart(testCtx(), svc, "", 0, 0, trustRemoteCodeUnspecified)
	if err == nil {
		t.Fatal("expected serviceStart to reach the launch path and fail on the missing compose_file")
	}
	if !strings.Contains(err.Error(), "no compose_file defined") {
		t.Fatalf("expected the resolveComposePath error (launch path reached), got: %v", err)
	}
}

func mustAdoptPort(t *testing.T, name string) int {
	t.Helper()
	port, ok := status.AdoptableExternalEnginePort(name)
	if !ok {
		t.Fatalf("%q not adoptable", name)
	}
	return port
}
