package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
)

func TestUsageLabel(t *testing.T) {
	if got := usageLabel(status.ServiceStatusStopped, nil); got != "-" {
		t.Errorf("stopped usage = %q, want -", got)
	}
	if got := usageLabel(status.ServiceStatusRunning, nil); got != "unknown" {
		t.Errorf("running no-signal usage = %q, want unknown", got)
	}
	if got := usageLabel(status.ServiceStatusRunning, &status.IdleState{Idle: false}); got != "busy" {
		t.Errorf("running busy usage = %q, want busy", got)
	}
	if got := usageLabel(status.ServiceStatusRunning, &status.IdleState{Idle: true, IdleSeconds: 120}); got != "idle 2m" {
		t.Errorf("running idle usage = %q, want idle 2m", got)
	}
}

func TestFootprintLabel(t *testing.T) {
	if got := footprintLabel(nil); got != "-" {
		t.Errorf("nil footprint = %q, want -", got)
	}
	fp := &status.ServiceFootprint{RAMBytes: 6 * 1 << 30, HasGPU: false}
	if got := footprintLabel(fp); got == "-" || got == "" {
		t.Errorf("footprint should render, got %q", got)
	}
}

func TestCollectServiceRowsAnnotatesCandidatesAndSorts(t *testing.T) {
	heavyIdle := &status.IdleState{Idle: true, IdleSeconds: 3600}
	st := &status.NodeStatus{
		Services: []status.ServiceInfo{
			{Name: "vllm", Status: status.ServiceStatusRunning, IdleState: heavyIdle,
				Footprint: &status.ServiceFootprint{VRAMBytes: 21 * 1 << 30, HasGPU: true}},
		},
		Apps: []status.AppInfo{
			{Name: "diffusers", Status: status.ServiceStatusRunning,
				IdleState: &status.IdleState{Idle: false}},
		},
	}
	candidates := map[string]bool{"service/vllm": true}
	rows := collectServiceRows(st, candidates)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Sorted: apps ("app") before services ("service").
	if rows[0].name != "diffusers" || rows[1].name != "vllm" {
		t.Errorf("unexpected order: %s, %s", rows[0].name, rows[1].name)
	}
	if rows[1].note == "" {
		t.Errorf("vllm should be flagged (heavy+idle and candidate), note empty")
	}
	if rows[0].usage != "busy" {
		t.Errorf("diffusers usage = %q, want busy", rows[0].usage)
	}
}

func TestEnabledLabel(t *testing.T) {
	if enabledLabel(true) != "ENABLED" {
		t.Error("enabled true label wrong")
	}
	if enabledLabel(false) != "OFF (default)" {
		t.Error("enabled false label wrong")
	}
}
