package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
)

// TestApplyServiceFootprintsExcludesUnmanagedFromTotal is the citadel #805
// case: a detected/unmanaged row (e.g. an official Ollama running under its
// own bare container name, citadel #657) must still get its own per-row
// Footprint, but its RAM/VRAM must NOT be folded into the "managed: ..."
// roll-up total — that summary represents citadel-managed usage only.
func TestApplyServiceFootprintsExcludesUnmanagedFromTotal(t *testing.T) {
	data := &controlcenter.StatusData{
		Services: []controlcenter.ServiceInfo{
			{Name: "vllm", Status: "running", Managed: true},
			{Name: "ollama", Status: "running", Managed: false},
		},
		MemoryTotal: "62G",
	}
	nameToIdx := map[string]int{
		"citadel-vllm": 0,
		"ollama":       1,
	}
	footprints := map[string]status.ServiceFootprint{
		"citadel-vllm": {CPUPercent: 10, RAMBytes: 4 * gib},
		"ollama":       {CPUPercent: 5, RAMBytes: 8 * gib},
	}

	applyServiceFootprints(data, nameToIdx, footprints, status.NewFootprintIdleTracker())

	// Both rows get their own footprint rendered.
	if data.Services[0].Footprint == "" {
		t.Errorf("managed row should have a rendered footprint")
	}
	if data.Services[1].Footprint == "" {
		t.Errorf("unmanaged row should still have a rendered footprint")
	}

	// Only the managed row's 4G counts toward the total, not the unmanaged
	// row's 8G.
	if data.ManagedSummary != "managed: RAM 4.0G/62G" {
		t.Errorf("ManagedSummary = %q, want to include only the managed row's RAM (4.0G), not the unmanaged row's 8G", data.ManagedSummary)
	}
}

// TestApplyServiceFootprintsSumsOnlyManagedRows pins the multi-row case: two
// managed rows contribute to the total, one unmanaged row does not.
func TestApplyServiceFootprintsSumsOnlyManagedRows(t *testing.T) {
	data := &controlcenter.StatusData{
		Services: []controlcenter.ServiceInfo{
			{Name: "vllm", Status: "running", Managed: true},
			{Name: "bonsai", Status: "running", Managed: true},
			{Name: "ollama", Status: "running", Managed: false},
		},
	}
	nameToIdx := map[string]int{
		"citadel-vllm":   0,
		"citadel-bonsai": 1,
		"ollama":         2,
	}
	footprints := map[string]status.ServiceFootprint{
		"citadel-vllm":   {CPUPercent: 10, RAMBytes: 3 * gib},
		"citadel-bonsai": {CPUPercent: 1, RAMBytes: 2 * gib},
		"ollama":         {CPUPercent: 5, RAMBytes: 100 * gib},
	}

	applyServiceFootprints(data, nameToIdx, footprints, status.NewFootprintIdleTracker())

	if data.ManagedSummary != "managed: RAM 5.0G" {
		t.Errorf("ManagedSummary = %q, want managed: RAM 5.0G (3G+2G, excluding the unmanaged 100G row)", data.ManagedSummary)
	}
}

// TestApplyServiceFootprintsNoRunningRowsProducesZeroSummary covers the
// baseline: no rows resolved from footprints means an all-managed summary of
// zero, not a crash or stale total.
func TestApplyServiceFootprintsNoRunningRowsProducesZeroSummary(t *testing.T) {
	data := &controlcenter.StatusData{
		Services: []controlcenter.ServiceInfo{
			{Name: "vllm", Status: "stopped", Managed: true},
		},
	}

	applyServiceFootprints(data, map[string]int{}, map[string]status.ServiceFootprint{}, status.NewFootprintIdleTracker())

	if data.ManagedSummary != "managed: RAM 0.0G" {
		t.Errorf("ManagedSummary = %q, want managed: RAM 0.0G", data.ManagedSummary)
	}
}

const gib = uint64(1024 * 1024 * 1024)
