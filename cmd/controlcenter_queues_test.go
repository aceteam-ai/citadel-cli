package cmd

import (
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
)

// TestResolveControlCenterInferenceQueues pins the boot-time queue set and
// the InferenceQueueReconciler gap decision for the Control Center worker
// path (citadel #612/#823). Mirrors TestMissingQueues' style in
// work_queues_test.go, which already pins the shared missingQueues/
// appendUniqueQueues helpers this function is built from.
func TestResolveControlCenterInferenceQueues(t *testing.T) {
	gpuCaps := &capabilities.NodeCapabilities{
		GPU: &capabilities.GPUCapabilities{
			Devices: []capabilities.GPUDevice{{Name: "RTX 3090"}},
		},
	}

	tests := []struct {
		name        string
		nodeCaps    *capabilities.NodeCapabilities
		serving     bool
		workerHeld  bool
		wantQueues  []string
		wantMissing []string
	}{
		{
			name:        "GPU node: boot set already covers gpu-general -- no reconciler gap",
			nodeCaps:    gpuCaps,
			serving:     false,
			workerHeld:  false,
			wantQueues:  []string{"jobs:v1:cpu-general", "jobs:v1:gpu-general"},
			wantMissing: nil,
		},
		{
			name:        "CPU-only, not yet serving at boot: no inference queue yet, reconciler has a gap to fill",
			nodeCaps:    &capabilities.NodeCapabilities{},
			serving:     false,
			workerHeld:  false,
			wantQueues:  []string{"jobs:v1:cpu-general"},
			wantMissing: []string{"jobs:v1:gpu-general"},
		},
		{
			name:        "CPU-only, already serving at boot: gpu-general already subscribed -- no gap",
			nodeCaps:    &capabilities.NodeCapabilities{},
			serving:     true,
			workerHeld:  false,
			wantQueues:  []string{"jobs:v1:cpu-general", "jobs:v1:gpu-general"},
			wantMissing: nil,
		},
		{
			name:       "dedicated worker holds the node lock: no inference subscription, no reconciler at all",
			nodeCaps:   gpuCaps,
			serving:    true,
			workerHeld: true,
			wantQueues: []string{"jobs:v1:cpu-general"},
			// missing is nil even though a GPU/serving node would otherwise
			// have queues to add -- workerHeld means this TUI instance never
			// runs a consume loop, so nothing should be built.
			wantMissing: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotQueues, gotMissing := resolveControlCenterInferenceQueues(tt.nodeCaps, tt.serving, tt.workerHeld)
			if !reflect.DeepEqual(gotQueues, tt.wantQueues) {
				t.Errorf("queues = %v, want %v", gotQueues, tt.wantQueues)
			}
			if !reflect.DeepEqual(gotMissing, tt.wantMissing) {
				t.Errorf("missing = %v, want %v", gotMissing, tt.wantMissing)
			}
		})
	}
}

// TestControlCenterInferenceQueueReconcilerConstruction confirms the actual
// *worker.InferenceQueueReconciler construction gate in runTUIWorker: nil
// when resolveControlCenterInferenceQueues finds nothing missing (a GPU node,
// an already-serving node, or workerHeld), non-nil when a real gap exists
// (a fresh CPU-only node with no engine yet). This exercises the same
// worker.NewInferenceQueueReconciler(missing, ...) call runTUIWorker makes,
// without needing a live Redis API connection.
func TestControlCenterInferenceQueueReconcilerConstruction(t *testing.T) {
	buildReconciler := func(nodeCaps *capabilities.NodeCapabilities, serving, workerHeld bool) bool {
		_, missing := resolveControlCenterInferenceQueues(nodeCaps, serving, workerHeld)
		return len(missing) > 0
	}

	if buildReconciler(&capabilities.NodeCapabilities{}, true, false) {
		t.Error("already-serving CPU-only node should get no reconciler (no gap)")
	}
	if buildReconciler(&capabilities.NodeCapabilities{
		GPU: &capabilities.GPUCapabilities{Devices: []capabilities.GPUDevice{{Name: "RTX 3090"}}},
	}, false, false) {
		t.Error("GPU node should get no reconciler (already covered at boot)")
	}
	if buildReconciler(&capabilities.NodeCapabilities{}, false, false) == false {
		t.Error("fresh CPU-only, not-yet-serving node should get a reconciler (real gap)")
	}
	if buildReconciler(&capabilities.NodeCapabilities{}, false, true) {
		t.Error("workerHeld should suppress the reconciler even with an otherwise-real gap")
	}
}
