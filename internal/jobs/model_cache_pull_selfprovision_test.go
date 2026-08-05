// internal/jobs/model_cache_pull_selfprovision_test.go
//
// MODEL_CACHE_PULL for an engine whose compose owns its weights is a no-op
// SUCCESS, not an "unsupported engine" error (#666).
package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/services"
)

func pullResult(t *testing.T, out []byte) modelCachePullResult {
	t.Helper()
	var r modelCachePullResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("result is not valid JSON (%v): %s", err, out)
	}
	return r
}

// TestSelfProvisioningEngineIsSkippedNotRejected covers the reported symptom: a
// deploy of tei dispatched a MODEL_CACHE_PULL that failed with
// `unsupported engine "tei"`, sitting in the node log next to a perfectly
// successful SERVICE_START and looking like a real failure during triage.
func TestSelfProvisioningEngineIsSkippedNotRejected(t *testing.T) {
	h := &ModelCachePullHandler{}
	for engine := range selfProvisioningEngines {
		out, err := h.Execute(JobContext{}, &nexus.Job{
			ID:      "job-666",
			Type:    "MODEL_CACHE_PULL",
			Payload: map[string]string{"engine": engine, "model_name": "some/model"},
		})
		if err != nil {
			t.Errorf("engine %q: expected a no-op success, got error: %v", engine, err)
			continue
		}
		r := pullResult(t, out)
		if r.Status != "skipped" {
			t.Errorf("engine %q: status = %q, want \"skipped\"", engine, r.Status)
		}
		// The message is the point of the change: a bare success would be just as
		// confusing as the error it replaces, because the operator still could not
		// tell whether weights were fetched.
		if r.Message == "" {
			t.Errorf("engine %q: skipped result must explain why nothing was pulled", engine)
		}
		if r.Engine != engine {
			t.Errorf("engine %q: result engine = %q", engine, r.Engine)
		}
	}
}

// TestUnknownEngineStillErrors pins that this is an ALLOWLIST, not a blanket
// "anything unrecognised is fine". A typo in the engine name must still fail
// loudly -- otherwise the fix for a noisy log would convert every genuinely
// misrouted pull into a silent success, which is strictly worse.
func TestUnknownEngineStillErrors(t *testing.T) {
	h := &ModelCachePullHandler{}
	_, err := h.Execute(JobContext{}, &nexus.Job{
		ID:      "job-666-typo",
		Type:    "MODEL_CACHE_PULL",
		Payload: map[string]string{"engine": "teii", "model_name": "some/model"},
	})
	if err == nil {
		t.Fatal("an unrecognised engine must still be an error")
	}
	if !strings.Contains(err.Error(), "teii") {
		t.Errorf("error should name the offending engine, got: %v", err)
	}
}

// TestSelfProvisioningEnginesMatchTheirComposeFiles keeps the list anchored to
// things that are mechanically checkable.
//
// It deliberately does NOT try to assert "the compose pins a model". A first
// draft did, and it was wrong in two different ways at once: unlimited-ocr pins
// via `--model` (not `--model-id` or a MODEL= env), and kokoro pins nothing at
// all because its image serves one fixed model. Widening the pattern until both
// passed would have produced a regex that matches almost any compose -- a test
// that always passes, which is worse than no test because it reads like
// coverage.
//
// So this asserts the two properties that are unambiguous, and the human-written
// `reason` string carries the rest:
//   - the engine names a real embedded compose (catches a typo or a rename), and
//   - that compose mounts a cache directory, i.e. the container has somewhere to
//     download into, which is the shape of "it fetches its own weights".
func TestSelfProvisioningEnginesMatchTheirComposeFiles(t *testing.T) {
	for engine, reason := range selfProvisioningEngines {
		compose, ok := services.ServiceMap[engine]
		if !ok {
			t.Errorf("engine %q is on the self-provisioning list but has no embedded compose file", engine)
			continue
		}
		mountsCache := strings.Contains(compose, "citadel-cache") ||
			strings.Contains(compose, "HUGGINGFACE_HUB_CACHE") ||
			strings.Contains(compose, ".cache/huggingface")
		if !mountsCache {
			t.Errorf("engine %q claims to fetch its own weights but its compose mounts no cache directory "+
				"-- either the compose changed or the list is wrong", engine)
		}
		if reason == "" {
			t.Errorf("engine %q has no reason string; the reason is what a reviewer checks, "+
				"since the compose test cannot prove the claim on its own", engine)
		}
	}
}

// TestPullEnginesAreNotOnTheSkipList is the other direction. An engine this
// handler actually pulls for must never appear on the no-op list: that would
// turn a required download into a silent success and leave the engine starting
// against weights that were never fetched.
func TestPullEnginesAreNotOnTheSkipList(t *testing.T) {
	for _, engine := range []string{"ollama", "vllm", "llamacpp", "bonsai"} {
		if _, bad := selfProvisioningEngines[engine]; bad {
			t.Errorf("%q has a real pull implementation and must not be on the self-provisioning list", engine)
		}
	}
}
