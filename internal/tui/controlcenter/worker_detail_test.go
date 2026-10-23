package controlcenter

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/rivo/tview"
)

func sharedWorkerFixture() status.WorkerDetail {
	observed := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return status.WorkerDetail{
		Mode: "worker", Connected: true, NodeName: "selected-node", MeshIP: "100.64.0.58",
		Version: "2.201.0", PID: 42, UptimeSec: 120, ObservedAt: observed,
		Worker: &status.WorkerLiveness{Consuming: true, LastPollAt: &observed, LastJobConsumedAt: &observed, InFlight: 1, Processed: 7, Failed: 2, PubSubTransport: "websocket"},
		Swap: &status.SwapActivity{SwapsPerHour: 3, EvictingSwapsPerHour: 1, MaxEvictingPerHour: 5},
		Lanes: []status.LaneActivity{{Lane: "unbounded", Queued: 2, Executing: 1, ExecCapacity: 1, BusySince: &observed}},
	}
}

func TestSharedWorkerDetailShowsObservedConsumptionSwapAndLanes(t *testing.T) {
	detail := sharedWorkerFixture()
	no, yes := false, true
	for _, pulled := range []*bool{nil, &no, &yes} {
		detail.Swap.Recent = append(detail.Swap.Recent, status.SwapRecord{
			Backend: "ollama", Model: "qwen", Evicted: []string{"vllm"},
			StartedAt: detail.ObservedAt, Wait: time.Second, Outcome: "ready", Pulled: pulled,
		})
	}
	if err := status.ValidateWorkerDetail(detail); err != nil {
		t.Fatal("invalid display fixture", err)
	}
	text := sharedWorkerDetailText(&detail, detail.ObservedAt, detail.ObservedAt.Add(2*time.Second))
	for _, want := range []string{
		`Owner: PID 42, mode "worker", version "2.201.0"`,
		`Node: "selected-node", network IP "100.64.0.58", connected: true`,
		"Received: 2s ago", "Consumption: polling recently", "Last poll: 2026-10-23T12:00:00Z",
		"last consumed job: 2026-10-23T12:00:00Z", "In flight: 1; processed: 7; failed: 2",
		"Swaps/hour: 3; evicting/hour: 1; evicting limit: 5", "Pulled: unknown", "Pulled: false", "Pulled: true",
		`Evicted: "vllm"`, `Lane "unbounded": 2 queued, 1 executing, capacity 1; busy since 2026-10-23T12:00:00Z`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in display:\n%s", want, text)
		}
	}
	detail.Worker.Consuming = false
	if text := sharedWorkerDetailText(&detail, detail.ObservedAt, detail.ObservedAt); !strings.Contains(text, "not polling recently (not proof of stopped work)") || strings.Contains(text, "WORKER ACTIVE") {
		t.Fatal("nonconsuming external worker displayed as active or stopped", text)
	}
}

func TestSharedWorkerUnavailableClearsHealthyLookingLocalFallbacks(t *testing.T) {
	cc := &ControlCenter{data: StatusData{
		SharedWorker: true, WorkerRunning: true, WorkerQueue: "stale-local-queue",
		Jobs: JobStats{Completed: 99},
	}}
	text := cc.getJobsText()
	if !strings.Contains(text, "Worker detail unavailable") || !strings.Contains(text, "unknown") {
		t.Fatal("unavailable external detail missing explicit unknown state", text)
	}
	for _, stale := range []string{"ACTIVE", "stopped", "stale-local-queue", "99"} {
		if strings.Contains(text, stale) {
			t.Fatal("unavailable external detail retained local state", text)
		}
	}
	detail := sharedWorkerFixture()
	detail.Worker, detail.Swap, detail.Lanes = nil, nil, nil
	text = sharedWorkerDetailText(&detail, time.Time{}, detail.ObservedAt)
	for _, want := range []string{"Consumption: unknown", "Swaps: unknown", "Lanes: unknown", "Receipt freshness: unknown"} {
		if !strings.Contains(text, want) {
			t.Fatal("absent provider treated as healthy zero", text)
		}
	}
	detail.Lanes = []status.LaneActivity{}
	if text := sharedWorkerDetailText(&detail, detail.ObservedAt.Add(time.Second), detail.ObservedAt); !strings.Contains(text, "Lanes: none") || !strings.Contains(text, "Receipt freshness: unknown") {
		t.Fatal("known empty lanes or future receipt lost", text)
	}
	cc.data.Connected, cc.data.WorkerRunning = true, false
	cc.data.Services = []ServiceInfo{{Name: "ollama", Status: "running"}}
	if text := cc.getContextualSuggestion(); strings.Contains(text, "Worker not running") {
		t.Fatal("unknown external consumption claims worker stopped", text)
	}
}

func TestSharedWorkerPeerTextIsLiteralAndMarkupEscaped(t *testing.T) {
	detail := sharedWorkerFixture()
	detail.NodeName = "[red]\x1b[2J\n\u202e"
	detail.Swap.Recent = []status.SwapRecord{{Backend: "ollama", Model: "[green]\tweight", StartedAt: detail.ObservedAt, Outcome: "ready"}}
	plain := sharedWorkerDetailText(&detail, detail.ObservedAt, detail.ObservedAt)
	if !strings.Contains(plain, `"[red]\x1b[2J\n\u202e"`) || !strings.Contains(plain, `"[green]\tweight"`) {
		t.Fatal("peer text not rendered as literal ASCII", plain)
	}
	styled := renderSharedWorkerDetail(&detail, detail.ObservedAt, detail.ObservedAt)
	if strings.ContainsAny(styled, "\x1b\t\u202e") || strings.Contains(styled, "[red]") || strings.Contains(styled, "[green]") {
		t.Fatal("peer control/styling escaped the literal display", styled)
	}
	if !strings.Contains(styled, "[red[]") || !strings.Contains(styled, "[green[]") {
		t.Fatal("peer tview tags were dropped instead of escaped", styled)
	}
	detail.Version = strings.Repeat("sensitive", 40)
	text := renderSharedWorkerDetail(&detail, detail.ObservedAt, detail.ObservedAt)
	if !strings.Contains(text, "Worker detail unavailable") || strings.Contains(text, "sensitive") || strings.Contains(text, "selected-node") {
		t.Fatal("invalid detail was rendered or leaked", text)
	}
}

func TestSharedWorkerRendersCompleteBoundedSwapHistory(t *testing.T) {
	detail := sharedWorkerFixture()
	for i := 0; i < 64; i++ {
		detail.Swap.Recent = append(detail.Swap.Recent, status.SwapRecord{Backend: "ollama", Model: fmt.Sprintf("model-%02d", i), StartedAt: detail.ObservedAt, Outcome: "ready"})
	}
	text := sharedWorkerDetailText(&detail, detail.ObservedAt, detail.ObservedAt)
	if strings.Count(text, "Pulled: unknown") != 64 || !strings.Contains(text, `model "model-63"`) {
		t.Fatal("bounded swap history silently truncated", text)
	}
	cc := &ControlCenter{data: StatusData{WorkerRunning: true, WorkerQueue: "standalone"}}
	if text := cc.getJobsText(); !strings.Contains(text, "Worker: ACTIVE") || !strings.Contains(text, "standalone") {
		t.Fatal("standalone jobs text changed", text)
	}
}

func TestSharedWorkerModalClearsAnObservationAfterReadFailure(t *testing.T) {
	detail := sharedWorkerFixture()
	cc := &ControlCenter{
		inModal: true, sharedWorkerDetailView: tview.NewTextView(),
		data: StatusData{SharedWorker: true, WorkerDetail: &detail, WorkerDetailObservedAt: detail.ObservedAt},
	}
	cc.updateSharedWorkerDetailModal()
	if text := cc.sharedWorkerDetailView.GetText(false); !strings.Contains(text, "selected-node") || !strings.Contains(text, "polling recently") {
		t.Fatal("modal never displayed initial valid observation", text)
	}
	cc.data.WorkerDetail = nil // The monitor clears its snapshot after a failed read.
	cc.updateSharedWorkerDetailModal()
	if text := cc.sharedWorkerDetailView.GetText(false); !strings.Contains(text, "Worker detail unavailable") || strings.Contains(text, "selected-node") || strings.Contains(text, "polling recently") {
		t.Fatal("modal retained a stale owner or consumption state", text)
	}
	cc.data.WorkerDetail, cc.data.SharedWorker = &detail, false
	cc.updateSharedWorkerDetailModal()
	if text := cc.sharedWorkerDetailView.GetText(false); strings.Contains(text, "selected-node") {
		t.Fatal("closed monitor displayed an old owner in its still-open modal", text)
	}
}
