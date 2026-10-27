package cmd

import (
	"sync/atomic"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

// These read-only closures are constructed before any shared status consumer
// starts. They read the same state as heartbeat without a collector, subscription
// or probe. A subsystem not yet initialized is unavailable, never a healthy zero.
func newWorkSessionProviders(state *worker.WorkerState, swaps *atomic.Pointer[worker.SwapManager], runner *atomic.Pointer[worker.Runner], transport func() string) nodeSessionWorkerProviders {
	return nodeSessionWorkerProviders{
		Worker: func() *status.WorkerLiveness {
			if state == nil || runner == nil || runner.Load() == nil {
				return nil
			}
			live := workerLivenessFrom(state.Snapshot())
			if transport != nil {
				live.PubSubTransport = transport()
			}
			return live
		},
		Swap: func() *status.SwapActivity {
			if swaps == nil {
				return nil
			}
			manager := swaps.Load()
			if manager == nil {
				return nil
			}
			return swapStatsFrom(manager.SwapStats())
		},
		Lanes: func() []status.LaneActivity {
			if runner == nil {
				return nil
			}
			current := runner.Load()
			if current == nil {
				return nil
			}
			return laneActivityFrom(current.LaneSnapshots())
		},
	}
}
