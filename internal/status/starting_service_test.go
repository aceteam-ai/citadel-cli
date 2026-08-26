package status

import (
	"context"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// starting_service_test.go: a container that is UP but not answering yet must be
// REPORTED, with its readiness on Health (aceteam#7148).
//
// The production repro: a deploy of gte-multilingual-base to node 1314 started
// TEI, `service_status(1314, "tei")` said "tei is running (docker)", and
// `fabric_node_status(1314)` said "Running Services: none" for the whole time
// the model was downloading. The heartbeat dropped any service that failed its
// probe, so the two views disagreed and the operator saw an empty node right
// after a deploy that had worked.

// neverHealthy is the injected stand-in for a readiness probe that has not
// passed yet: the container is up, the HTTP endpoint is not answering.
func neverHealthy(int) bool { return false }

func TestCollectEmbeddingServices_WarmingIsReportedAsStarting(t *testing.T) {
	running := func(name string) (int, bool) { return managedEngineHostPort("tei"), name == "tei" }
	// A warming server answers nothing, so the lister must never be consulted.
	// Handing it a model it would report proves the warming branch cannot leak
	// one into the heartbeat.
	md := stubEmbeddingLister{models: []string{"should-never-be-reported"}}

	got := collectEmbeddingServices(context.Background(), running, neverHealthy, md)

	if len(got) != 1 {
		t.Fatalf("a running-but-warming tei must still be reported, got %d entries", len(got))
	}
	if got[0].Status != ServiceStatusRunning {
		t.Errorf("status = %q, want %q (the container really is running)", got[0].Status, ServiceStatusRunning)
	}
	if got[0].Health != HealthStatusStarting {
		t.Errorf("health = %q, want %q", got[0].Health, HealthStatusStarting)
	}
	if len(got[0].Models) != 0 {
		t.Errorf("a warming service must claim no models, got %v", got[0].Models)
	}
	// citadel-cli#684: Readiness is additive on top of the Status/Health
	// assertions above (unchanged).
	if got[0].Readiness != ReadinessStarting {
		t.Errorf("readiness = %q, want %q", got[0].Readiness, ReadinessStarting)
	}
	if got[0].Reason == "" {
		t.Error("expected a non-empty reason for the starting classification")
	}
	if got[0].ProbedAt == nil {
		t.Error("expected ProbedAt to be set: a live /health probe did run this cycle")
	}
}

// The ready case still reports Health=ok and the served model, so making the
// warming case visible did not weaken the readiness signal the platform's
// embedding resolver gates on.
func TestCollectEmbeddingServices_ReadyReportsModelAndOK(t *testing.T) {
	running := func(name string) (int, bool) { return managedEngineHostPort("tei"), name == "tei" }
	md := stubEmbeddingLister{models: []string{"Alibaba-NLP/gte-multilingual-base"}}

	got := collectEmbeddingServices(context.Background(), running, alwaysHealthy, md)

	if len(got) != 1 {
		t.Fatalf("expected one tei entry, got %d", len(got))
	}
	if got[0].Health != HealthStatusOK {
		t.Errorf("health = %q, want %q once /health answers 200", got[0].Health, HealthStatusOK)
	}
	if len(got[0].Models) != 1 || got[0].Models[0] != "Alibaba-NLP/gte-multilingual-base" {
		t.Errorf("models = %v, want the served TEI model id", got[0].Models)
	}
	// citadel-cli#684: Readiness is additive on top of the Status/Health
	// assertions above (unchanged).
	if got[0].Readiness != ReadinessReady {
		t.Errorf("readiness = %q, want %q", got[0].Readiness, ReadinessReady)
	}
	if got[0].Reason != "" {
		t.Errorf("reason = %q, want empty for the ready case", got[0].Reason)
	}
	if got[0].ProbedAt == nil {
		t.Error("expected ProbedAt to be set: a live /health probe did run this cycle")
	}
}

// The collector wrapper must thread the once-per-heartbeat running set into that
// same decision, so the seam the tests above use is the one production runs.
func TestCollectEmbeddingServiceStatus_UsesRunningSet(t *testing.T) {
	c := &Collector{}

	got := c.collectEmbeddingServiceStatus(map[string]bool{"tei": true})

	if len(got) != 1 || got[0].Name != "tei" {
		t.Fatalf("tei in the running set must be reported, got %+v", got)
	}
	if got[0].Status != ServiceStatusRunning {
		t.Errorf("status = %q, want %q", got[0].Status, ServiceStatusRunning)
	}
	// Health is whichever branch the live /health probe took on this machine.
	// Both are valid; what must never happen again is the entry being absent, or
	// carrying a health value that says nothing about readiness.
	if got[0].Health != HealthStatusOK && got[0].Health != HealthStatusStarting {
		t.Errorf("health = %q, want %q or %q", got[0].Health, HealthStatusOK, HealthStatusStarting)
	}
}

// TestCollectManagedEngineStatus_UnresponsiveEngineIsStarting covers the same
// gap for serving engines: a vLLM whose container is up while it loads weights
// used to vanish from the heartbeat entirely.
func TestCollectManagedEngineStatus_UnresponsiveEngineIsStarting(t *testing.T) {
	// No idleTracker and no modelDiscovery => no probe can respond, which is
	// exactly the shape of an engine that is up but not serving yet.
	c := &Collector{}

	// Other entries can appear from the native-engine fallback if the machine
	// running the tests happens to serve one, so select the engine under test
	// rather than asserting on the slice length.
	got := c.collectManagedEngineStatus(map[string]bool{"vllm": true})
	var vllm *ServiceInfo
	for i := range got {
		if got[i].Name == "vllm" {
			vllm = &got[i]
			break
		}
	}
	if vllm == nil {
		t.Fatal("a running-but-unresponsive vllm must still be reported")
	}
	if vllm.Status != ServiceStatusRunning {
		t.Errorf("status = %q, want %q", vllm.Status, ServiceStatusRunning)
	}
	if vllm.Health != HealthStatusStarting {
		t.Errorf("health = %q, want %q", vllm.Health, HealthStatusStarting)
	}
	if vllm.IdleState != nil {
		t.Error("an unresponsive engine must not claim an idle signal")
	}
	// citadel-cli#684: Readiness is purely additive on top of the Status/Health
	// assertions above, which are unchanged. A container that is up but not yet
	// answering must classify as starting, never stopped/down.
	if vllm.Readiness != ReadinessStarting {
		t.Errorf("readiness = %q, want %q", vllm.Readiness, ReadinessStarting)
	}
	if vllm.Reason == "" {
		t.Error("expected a non-empty reason for the starting classification")
	}
}

// The probe lists are now filtered through services.ServiceMap by
// runningEmbeddedServices, which the per-engine `docker inspect` they replaced
// was not. A probe engine missing from ServiceMap would silently lose container
// detection and survive only on the native port fallback, so pin the subset.
func TestProbeListsAreSubsetsOfServiceMap(t *testing.T) {
	probed := append(append([]string{}, managedProbeEngines...), embeddingProbeServices...)
	for _, name := range probed {
		if _, ok := services.ServiceMap[name]; !ok {
			t.Errorf("probe engine %q is not in services.ServiceMap, so its container can never be enumerated", name)
		}
	}
}
