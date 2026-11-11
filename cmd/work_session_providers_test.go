package cmd

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
)

func TestWorkSessionProvidersReadChangingSharedState(t *testing.T) {
	state := worker.NewWorkerState()
	var swaps atomic.Pointer[worker.SwapManager]
	var runner atomic.Pointer[worker.Runner]
	transport := "websocket"
	providers := newWorkSessionProviders(state, &swaps, &runner, func() string { return transport })
	if providers.Worker() != nil || providers.Swap() != nil || providers.Lanes() != nil {
		t.Fatal("uninitialized worker subsystems looked available")
	}
	runner.Store(worker.NewRunner(nil, nil, worker.RunnerConfig{State: state}))
	state.RecordPoll()
	first := providers.Worker()
	if first == nil || !first.Consuming || first.LastPollAt == nil || first.PubSubTransport != "websocket" {
		t.Fatalf("live worker missing after initialization: %+v", first)
	}
	transport = "http"
	state.RecordJobReceived()
	second := providers.Worker()
	if second.PubSubTransport != "http" || second.LastJobConsumedAt == nil || first.LastJobConsumedAt != nil {
		t.Fatal("provider cached state instead of reading the same live worker")
	}
	*second.LastPollAt = second.LastPollAt.AddDate(-1, 0, 0)
	if providers.Worker().LastPollAt.Equal(*second.LastPollAt) {
		t.Fatal("caller mutation reached worker-owned timestamp")
	}
	lanes := providers.Lanes()
	if len(lanes) < 2 || lanes[0].ExecCapacity <= 0 {
		t.Fatal("live runner lane projection missing")
	}
	lanes[0].Lane = "changed-by-reader"
	if providers.Lanes()[0].Lane == lanes[0].Lane {
		t.Fatal("caller mutation reached runner-owned lane")
	}
	swaps.Store(worker.NewSwapManager(stubSwapController{}))
	if providers.Swap() == nil {
		t.Fatal("same provider did not observe late swap initialization")
	}
	swaps.Store(nil)
	runner.Store(nil)
	if providers.Worker() != nil || providers.Swap() != nil || providers.Lanes() != nil {
		t.Fatal("removed subsystem retained stale detail")
	}
}

func TestHeldWorkerMonitorPrecedesWorkerOwnedConstruction(t *testing.T) {
	source, err := os.ReadFile("controlcenter.go")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(source), "func runTUIWorker(")
	if start < 0 {
		t.Fatal("worker entrypoint missing")
	}
	body := string(source[start:])
	monitor := strings.Index(body, "return runControlCenterWorkerMonitor(ctx, network.GetStateDir(), workerPID, network.SessionControlWorkerDetailRequest)")
	if monitor < 0 || !strings.Contains(body[:monitor], "if workerHeld {") {
		t.Fatal("held-worker path no longer selects the shared read-only monitor")
	}
	for _, constructor := range []string{"startClockMonitor(", "getDeviceConfigFromFile(", "update.ReconcilePendingOnStartup(", "worker.NewAPISource(", "status.NewCollector(", "heartbeat.NewAPIPublisher(", "nodestate.New(", "worker.NewRunner("} {
		position := strings.Index(body, constructor)
		if position < monitor {
			t.Fatalf("held monitor can reach %s before returning", constructor)
		}
	}
	work, err := os.ReadFile("work.go")
	if err != nil {
		t.Fatal(err)
	}
	wiring := string(work)
	bundle := strings.Index(wiring, "sessionProviders := newWorkSessionProviders(")
	publish := strings.Index(wiring, "startNodeSessionControl(workSessionMode, cancel, sessionProviders)")
	if bundle < 0 || publish < bundle || !strings.Contains(wiring, "workerLivenessFn, swapStatsFn, laneActivityFn := sessionProviders.Worker, sessionProviders.Swap, sessionProviders.Lanes") {
		t.Fatal("session control is not wired to the immutable heartbeat provider bundle")
	}
}
