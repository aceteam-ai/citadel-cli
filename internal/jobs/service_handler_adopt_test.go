package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

	t.Run("undeterminable ownership (no runtime CLI) does not adopt", func(t *testing.T) {
		// No dockerServiceRunningFn seam -> ourContainerRunning does the real
		// exec.LookPath, which fails with PATH neutered, so ownership is
		// undeterminable and adoption declines (the production fail-safe that also
		// keeps neutered-PATH docker-branch tests hermetic).
		t.Setenv("PATH", t.TempDir())
		h := NewServiceHandler(t.TempDir())
		h.externalEngineServingFn = func(_ context.Context, _ string, _ int) ([]string, bool) {
			return []string{"Qwen/Qwen3-8B"}, true
		}
		if _, adopted := h.maybeAdoptExternalEngine(testCtx(), svc, "", "docker"); adopted {
			t.Fatal("expected adopted=false when the container runtime is unavailable")
		}
	})
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
