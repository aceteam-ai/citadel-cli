package status

import (
	"net"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// readiness_test.go: table tests over the four-valued Readiness state matrix
// (citadel-cli#684) -- down/starting/ready, plus the pure decision helper --
// with Status/Health pinned unchanged alongside each case.

// TestReadinessForProbe_Matrix exercises readinessForProbe directly: no I/O,
// no live probe, so the down/starting/ready decision logic is pinned
// independent of any network or Collector wiring.
func TestReadinessForProbe_Matrix(t *testing.T) {
	cases := []struct {
		name           string
		responded      bool
		probeAttempted bool
		wantReadiness  string
		wantReasonSet  bool
		wantProbedAt   bool
	}{
		{
			name:           "probe answered",
			responded:      true,
			probeAttempted: true,
			wantReadiness:  ReadinessReady,
			wantReasonSet:  false,
			wantProbedAt:   true,
		},
		{
			name:           "probe attempted but timed out",
			responded:      false,
			probeAttempted: true,
			wantReadiness:  ReadinessStarting,
			wantReasonSet:  true,
			wantProbedAt:   true,
		},
		{
			name:           "no probe available at all",
			responded:      false,
			probeAttempted: false,
			wantReadiness:  ReadinessStarting,
			wantReasonSet:  true,
			wantProbedAt:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readiness, reason, probedAt := readinessForProbe(tc.responded, tc.probeAttempted)
			if readiness != tc.wantReadiness {
				t.Errorf("readiness = %q, want %q", readiness, tc.wantReadiness)
			}
			if (reason != "") != tc.wantReasonSet {
				t.Errorf("reason = %q, wantReasonSet=%v", reason, tc.wantReasonSet)
			}
			if (probedAt != nil) != tc.wantProbedAt {
				t.Errorf("probedAt set = %v, want %v", probedAt != nil, tc.wantProbedAt)
			}
		})
	}
}

// TestCollectManagedEngineStatus_ContainerDownYieldsNoEntry pins the existing
// (pre-#684, unchanged) contract: an engine whose container is NOT running is
// simply absent from collectManagedEngineStatus's output -- it is the disk
// branch (collectInstalledEngines, see hotswap_test.go) that reports a
// not-running engine, as ReadinessDown.
func TestCollectManagedEngineStatus_ContainerDownYieldsNoEntry(t *testing.T) {
	c := &Collector{}
	got := c.collectManagedEngineStatus(map[string]bool{}) // nothing running
	for _, svc := range got {
		if svc.Name == "vllm" {
			t.Fatalf("a non-running vllm must not be reported by collectManagedEngineStatus, got %+v", svc)
		}
	}
}

// TestCollectManagedEngineStatus_ReadyIsReportedWithProbedAtAndNoReason wires
// a real ModelDiscovery against an httptest-backed /v1/models endpoint to
// prove the "container up + probe answered" case end to end, not just via the
// pure helper above. It temporarily points citadel's vllm host-port registry
// at the test server's port (restored via t.Cleanup) since
// collectManagedEngineStatus resolves the probe port through that registry,
// not through an injectable parameter.
//
// Mutates the package-level services.ServiceHostPorts map, which is safe only
// because internal/status has no t.Parallel() calls today (this package runs
// its tests sequentially) and no other "vllm" test in this package expects a
// different port concurrently. If either changes, this test (and its sibling
// below) need a non-shared way to inject the probe port.
func TestCollectManagedEngineStatus_ReadyIsReportedWithProbedAtAndNoReason(t *testing.T) {
	discovery, port := newTestDiscovery(t, openAIModelsHandler("Qwen/Qwen3-8B"))

	orig, hadOrig := services.ServiceHostPorts["vllm"]
	services.ServiceHostPorts["vllm"] = port
	t.Cleanup(func() {
		if hadOrig {
			services.ServiceHostPorts["vllm"] = orig
		} else {
			delete(services.ServiceHostPorts, "vllm")
		}
	})

	c := &Collector{modelDiscovery: discovery}
	got := c.collectManagedEngineStatus(map[string]bool{"vllm": true})

	var vllm *ServiceInfo
	for i := range got {
		if got[i].Name == "vllm" {
			vllm = &got[i]
			break
		}
	}
	if vllm == nil {
		t.Fatal("expected a vllm entry")
	}
	// Status/Health: unchanged pre-#684 values for the already-correctly-
	// classified "answered" case.
	if vllm.Status != ServiceStatusRunning {
		t.Errorf("status = %q, want %q", vllm.Status, ServiceStatusRunning)
	}
	if vllm.Health != HealthStatusOK {
		t.Errorf("health = %q, want %q", vllm.Health, HealthStatusOK)
	}
	if len(vllm.Models) != 1 || vllm.Models[0] != "Qwen/Qwen3-8B" {
		t.Errorf("models = %v, want [Qwen/Qwen3-8B]", vllm.Models)
	}
	// Readiness: additive.
	if vllm.Readiness != ReadinessReady {
		t.Errorf("readiness = %q, want %q", vllm.Readiness, ReadinessReady)
	}
	if vllm.Reason != "" {
		t.Errorf("reason = %q, want empty for the ready case", vllm.Reason)
	}
	if vllm.ProbedAt == nil {
		t.Error("expected ProbedAt to be set: a live model-discovery probe did run this cycle")
	}
}

// TestCollectManagedEngineStatus_UnansweredProbeIsStartingNotStopped is the
// direct regression test for the issue's headline bug: a container that is up
// but whose model-discovery probe does not answer must classify as starting,
// carrying a reason, never as stopped/unknown (which is what the disk branch
// used to re-classify it as before the container-up check was added).
//
// This exercises the CONNECTION-REFUSED failure mode specifically (bind then
// immediately close a listener, same trick as TestDiscoverModels_Unreachable
// in models_test.go) rather than an actual context.DeadlineExceeded, because
// refused is the common real-world case: for a real weights load (e.g. vLLM
// importing Python before uvicorn ever binds the port) nothing is listening
// for most of the load, not merely responding slowly. That is exactly why the
// Reason text says "did not return a served model within Ns" rather than
// "timed out" -- a refused connection fails immediately, well inside the
// probe's timeout bound, and readinessForProbe's Reason must stay true for
// all three DiscoverModels failure modes (refused, non-200, and a genuine
// deadline), not just this one.
func TestCollectManagedEngineStatus_UnansweredProbeIsStartingNotStopped(t *testing.T) {
	discovery := NewModelDiscovery()
	discovery.host = "127.0.0.1"
	deadPort := reserveDeadPort(t)

	orig, hadOrig := services.ServiceHostPorts["vllm"]
	services.ServiceHostPorts["vllm"] = deadPort
	t.Cleanup(func() {
		if hadOrig {
			services.ServiceHostPorts["vllm"] = orig
		} else {
			delete(services.ServiceHostPorts, "vllm")
		}
	})

	c := &Collector{modelDiscovery: discovery}
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
	// Status/Health: unchanged pre-#684 values (Status stays "running" -- the
	// container really is up -- Health becomes "starting", the aceteam#7148
	// behavior this PR does not alter).
	if vllm.Status != ServiceStatusRunning {
		t.Errorf("status = %q, want %q", vllm.Status, ServiceStatusRunning)
	}
	if vllm.Health != HealthStatusStarting {
		t.Errorf("health = %q, want %q", vllm.Health, HealthStatusStarting)
	}
	if vllm.Status == ServiceStatusStopped {
		t.Error("a container that is up must never be reported stopped, regardless of probe outcome")
	}
	// Readiness: additive, and this is the exact case the issue calls out.
	if vllm.Readiness != ReadinessStarting {
		t.Errorf("readiness = %q, want %q", vllm.Readiness, ReadinessStarting)
	}
	if !strings.Contains(vllm.Reason, "did not return a served model") {
		t.Errorf("reason = %q, want it to explain the unanswered probe without claiming a timeout", vllm.Reason)
	}
	if vllm.ProbedAt == nil {
		t.Error("expected ProbedAt to be set: a live model-discovery probe did run this cycle")
	}
}

// reserveDeadPort binds then immediately closes a loopback TCP listener,
// returning a port number that is reserved-but-dead: connecting to it fails
// fast (connection refused) rather than hanging, without needing a live
// server or the full probe timeout. Mirrors TestDiscoverModels_Unreachable in
// models_test.go.
func reserveDeadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}
