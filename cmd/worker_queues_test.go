// cmd/worker_queues_test.go
package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/capabilities"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// TestResolveWorkerQueues_FetchUnavailable pins the boot-time queue set and
// the InferenceQueueReconciler gap decision when FetchWorkerConfig cannot be
// reached (WorkQueue and OrgID both stay empty, degrading gracefully to
// defaults -- the "using defaults" branch runWork's original inline comment
// described). Mirrors the pre-#839 resolveControlCenterInferenceQueues
// matrix (GPU node, CPU-only not yet serving, CPU-only already serving), now
// exercised through the single shared resolver both runWork and
// runTUIWorker call. APIBaseURL points at an address nothing listens on, so
// this stays hermetic (no real network dependency) while still exercising
// the actual FetchWorkerConfig call path rather than bypassing it.
//
// With no workQueue and no orgID, the returned Queues slice deliberately does
// NOT get an explicit jobs:v1:cpu-general entry prepended -- that mirrors
// runWork's original inline behavior exactly (apiQueueNames starts nil; only
// the shellQueue branch, when orgID is known, injects the explicit base so
// appending the shell queue doesn't suppress it). An empty/nil Queues slice
// is not a bug: worker.NewAPISource's own zero-value default already falls
// back to worker.DefaultCPUQueue (see api_source.go), so the two forms
// resolve to an identical effective subscription either way.
func TestResolveWorkerQueues_FetchUnavailable(t *testing.T) {
	gpuCaps := &capabilities.NodeCapabilities{
		GPU: &capabilities.GPUCapabilities{
			Devices: []capabilities.GPUDevice{{Name: "RTX 3090"}},
		},
	}

	tests := []struct {
		name        string
		nodeCaps    *capabilities.NodeCapabilities
		serving     bool
		orgID       string
		wantQueues  []string
		wantMissing []string
	}{
		{
			name:        "GPU node, no org: boot set already covers gpu-general -- no reconciler gap",
			nodeCaps:    gpuCaps,
			serving:     false,
			wantQueues:  []string{"jobs:v1:gpu-general"},
			wantMissing: nil,
		},
		{
			name:        "CPU-only, not yet serving at boot, no org: no inference queue yet, gap to fill",
			nodeCaps:    &capabilities.NodeCapabilities{},
			serving:     false,
			wantQueues:  nil,
			wantMissing: []string{"jobs:v1:gpu-general"},
		},
		{
			name:        "CPU-only, already serving at boot, no org: gpu-general already subscribed -- no gap",
			nodeCaps:    &capabilities.NodeCapabilities{},
			serving:     true,
			wantQueues:  []string{"jobs:v1:gpu-general"},
			wantMissing: nil,
		},
		{
			name:        "GPU node with org: shell queue appended alongside the GPU inference queue",
			nodeCaps:    gpuCaps,
			serving:     false,
			orgID:       "org-1",
			wantQueues:  []string{worker.DefaultCPUQueue, "jobs:v1:shell:org_org-1", "jobs:v1:gpu-general"},
			wantMissing: nil,
		},
	}

	// Port 1 is reserved; nothing listens there, so FetchWorkerConfig fails
	// fast with a connection error on every case below without an external
	// network dependency or a real timeout wait.
	const unreachableAPIBaseURL = "http://127.0.0.1:1"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveWorkerQueues(context.Background(), workerQueueParams{
				APIBaseURL: unreachableAPIBaseURL,
				Token:      "test-token",
				OrgID:      tt.orgID,
				NodeCaps:   tt.nodeCaps,
				Serving:    tt.serving,
			})
			if !reflect.DeepEqual(got.Queues, tt.wantQueues) {
				t.Errorf("queues = %v, want %v", got.Queues, tt.wantQueues)
			}
			if !reflect.DeepEqual(got.Missing, tt.wantMissing) {
				t.Errorf("missing = %v, want %v", got.Missing, tt.wantMissing)
			}
		})
	}
}

// TestResolveWorkerQueues_WorkerHeld pins the workerHeld short-circuit: when
// WorkerHeld is set (runTUIWorker's dedicated-worker-holds-the-lock case), no
// FetchWorkerConfig call is made (no APIBaseURL/Token needed), no shell
// queue or inference queue is added, and no reconciler gap is reported --
// regardless of what NodeCaps/Serving/OrgID would otherwise produce. This is
// the "one input the two entry points legitimately differ on" the issue asks
// to model explicitly: runWork never sets it (always false); runTUIWorker
// sets it from its own workerHeld detection.
func TestResolveWorkerQueues_WorkerHeld(t *testing.T) {
	gpuCaps := &capabilities.NodeCapabilities{
		GPU: &capabilities.GPUCapabilities{
			Devices: []capabilities.GPUDevice{{Name: "RTX 3090"}},
		},
	}

	got := resolveWorkerQueues(context.Background(), workerQueueParams{
		OrgID:      "org-1",
		NodeCaps:   gpuCaps,
		Serving:    true,
		WorkerHeld: true,
	})

	want := workerQueueResult{
		OrgID:  "org-1",
		Queues: []string{worker.DefaultCPUQueue},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveWorkerQueues(WorkerHeld=true) = %+v, want %+v", got, want)
	}
}

// TestResolveWorkerQueues_FetchWorkerConfig exercises the FetchWorkerConfig
// round-trip (WorkQueue=="" and OrgID==""): the resolved workQueue and orgID
// come from the server response, the shell queue is derived from the
// resolved orgID, and the reconciler gap is computed against the resulting
// set.
func TestResolveWorkerQueues_FetchWorkerConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(redisapi.WorkerConfigResponse{
			Queue: "jobs:v1:platform-assigned",
			OrgID: "org-777",
		})
	}))
	defer srv.Close()

	got := resolveWorkerQueues(context.Background(), workerQueueParams{
		APIBaseURL: srv.URL,
		Token:      "test-token",
		NodeCaps:   &capabilities.NodeCapabilities{},
		Serving:    false,
	})

	if got.WorkQueue != "jobs:v1:platform-assigned" {
		t.Errorf("WorkQueue = %q, want %q", got.WorkQueue, "jobs:v1:platform-assigned")
	}
	if got.OrgID != "org-777" {
		t.Errorf("OrgID = %q, want %q", got.OrgID, "org-777")
	}
	wantQueues := []string{"jobs:v1:platform-assigned", "jobs:v1:shell:org_org-777"}
	if !reflect.DeepEqual(got.Queues, wantQueues) {
		t.Errorf("queues = %v, want %v", got.Queues, wantQueues)
	}
	wantMissing := []string{"jobs:v1:gpu-general"}
	if !reflect.DeepEqual(got.Missing, wantMissing) {
		t.Errorf("missing = %v, want %v", got.Missing, wantMissing)
	}
}

// TestResolveWorkerQueuesRepresentativeConfig is citadel-cli#839's literal
// test ask: call the shared helper with a representative config -- a
// FetchWorkerConfig-fetched workQueue + orgID, plus a GPU node already
// serving -- and pin the resolved set.
//
// This does NOT simulate runWork and runTUIWorker as two independently
// re-derived call sites and diff their outputs -- doing so with hand-written
// duplicate literals in a test would be tautological (identical inputs
// trivially produce identical outputs; it proves nothing about the
// production call sites, which are what could actually drift). What
// structurally prevents that drift is that cmd/work.go's runWork and
// cmd/controlcenter.go's runTUIWorker both call this ONE function -- there
// is no second implementation left to diverge. See resolveWorkerQueues' and
// workerQueueParams' doc comments for that contract, and
// TestResolveWorkerQueues_WorkerHeld above for the one input (WorkerHeld)
// the two call sites are documented to legitimately differ on.
func TestResolveWorkerQueuesRepresentativeConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(redisapi.WorkerConfigResponse{
			Queue: "jobs:v1:platform-assigned",
			OrgID: "org-parity",
		})
	}))
	defer srv.Close()

	nodeCaps := &capabilities.NodeCapabilities{
		GPU: &capabilities.GPUCapabilities{
			Devices: []capabilities.GPUDevice{{Name: "RTX 4090"}},
		},
	}

	got := resolveWorkerQueues(context.Background(), workerQueueParams{
		APIBaseURL: srv.URL,
		Token:      "test-token",
		NodeCaps:   nodeCaps,
		Serving:    true,
		DebugFn:    Debug,
	})

	if got.WorkQueue != "jobs:v1:platform-assigned" {
		t.Errorf("WorkQueue = %q, want %q", got.WorkQueue, "jobs:v1:platform-assigned")
	}
	if got.OrgID != "org-parity" {
		t.Errorf("OrgID = %q, want %q", got.OrgID, "org-parity")
	}
	wantQueues := []string{"jobs:v1:platform-assigned", "jobs:v1:shell:org_org-parity", "jobs:v1:gpu-general"}
	if !reflect.DeepEqual(got.Queues, wantQueues) {
		t.Errorf("queues = %v, want %v", got.Queues, wantQueues)
	}
	if got.Missing != nil {
		t.Errorf("missing = %v, want nil (GPU node already covers gpu-general)", got.Missing)
	}

	// Determinism: a second call with the identical config must produce the
	// identical result (no hidden shared/mutable state inside the resolver).
	got2 := resolveWorkerQueues(context.Background(), workerQueueParams{
		APIBaseURL: srv.URL,
		Token:      "test-token",
		NodeCaps:   nodeCaps,
		Serving:    true,
		DebugFn:    func(format string, args ...any) {},
	})
	if !reflect.DeepEqual(got, got2) {
		t.Errorf("resolveWorkerQueues is not deterministic for the same config:\n  first:  %+v\n  second: %+v", got, got2)
	}
}
