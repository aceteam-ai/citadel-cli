package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/status"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
)

// TestMergeDetectedEnginesAddsUnmanagedRow is the core citadel #657 case: an
// engine running on the node (e.g. ollama installed by its own `curl | sh`
// script) that citadel never started must still show up in the Services pane,
// marked distinctly from a manifest/managed row.
func TestMergeDetectedEnginesAddsUnmanagedRow(t *testing.T) {
	managed := []controlcenter.ServiceInfo{
		{Name: "vllm", Status: "running", Managed: true},
	}
	discovered := []status.LocalEngine{
		{Name: "ollama", Port: 11434, Models: []string{"llama3"}},
	}

	got := mergeDetectedEngines(managed, discovered)

	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d: %+v", len(got), got)
	}
	if got[0].Name != "vllm" || !got[0].Managed {
		t.Errorf("managed row changed: %+v", got[0])
	}
	unmanaged := got[1]
	if unmanaged.Name != "ollama" {
		t.Fatalf("expected detected row named ollama, got %+v", unmanaged)
	}
	if unmanaged.Managed {
		t.Errorf("detected row must be marked Managed=false, got true")
	}
	if unmanaged.Status != "running" {
		t.Errorf("detected row status = %q, want running", unmanaged.Status)
	}
	if len(unmanaged.Models) != 1 || unmanaged.Models[0] != "llama3" {
		t.Errorf("detected row models = %v, want [llama3]", unmanaged.Models)
	}
}

// TestMergeDetectedEnginesDedupesManagedByName pins the de-dupe rule: an
// engine already represented by a RUNNING manifest/managed row (matched
// case-insensitively by name) must NOT also appear as a detected/unmanaged
// row, or a citadel-managed engine would be double-listed. The managed row
// wins unchanged when its compose stack is actually running.
func TestMergeDetectedEnginesDedupesManagedByName(t *testing.T) {
	managed := []controlcenter.ServiceInfo{
		{Name: "vllm", Status: "running", Managed: true},
		{Name: "Ollama", Status: "running", Managed: true}, // manifest casing may differ
	}
	discovered := []status.LocalEngine{
		{Name: "vllm", Port: 8100, Models: []string{"qwen"}},
		{Name: "ollama", Port: 11434, Models: []string{"llama3"}},
	}

	got := mergeDetectedEngines(managed, discovered)

	if len(got) != 2 {
		t.Fatalf("expected no new rows (both discovered engines already managed+running), got %d: %+v", len(got), got)
	}
	for _, svc := range got {
		if !svc.Managed {
			t.Errorf("row %q should remain Managed=true, was demoted to unmanaged", svc.Name)
		}
		if len(svc.Models) != 0 {
			t.Errorf("row %q should not gain Models from a discovered signal when the managed compose is already running, got %v", svc.Name, svc.Models)
		}
	}
}

// TestMergeDetectedEnginesUpgradesStoppedManagedRow is the citadel #804 case:
// citadel.yaml declares an engine (e.g. "ollama") but its compose stack is not
// running, while status.DiscoverLocalEngines' native-serving probe
// (nativesvc.IsNativeServiceServing) finds a live instance answering on the
// same engine's port — e.g. a separately-started native install. The live
// signal must upgrade the existing managed row (running state + models)
// rather than being silently dropped by the name-based de-dupe, and it must
// NOT also produce a second, duplicate row.
func TestMergeDetectedEnginesUpgradesStoppedManagedRow(t *testing.T) {
	managed := []controlcenter.ServiceInfo{
		{Name: "vllm", Status: "running", Managed: true},
		{Name: "Ollama", Status: "stopped", Managed: true}, // manifest casing may differ
	}
	discovered := []status.LocalEngine{
		{Name: "ollama", Port: 11434, Models: []string{"llama3"}},
	}

	got := mergeDetectedEngines(managed, discovered)

	if len(got) != 2 {
		t.Fatalf("expected the stopped row to be upgraded in place, not duplicated; got %d rows: %+v", len(got), got)
	}

	// The already-running vllm row must be untouched.
	if got[0].Name != "vllm" || got[0].Status != "running" || !got[0].Managed {
		t.Errorf("running managed row changed unexpectedly: %+v", got[0])
	}

	upgraded := got[1]
	if upgraded.Name != "Ollama" {
		t.Fatalf("expected the upgraded row to keep the manifest name %q, got %+v", "Ollama", upgraded)
	}
	if upgraded.Status != "running" {
		t.Errorf("upgraded row status = %q, want running", upgraded.Status)
	}
	if upgraded.Managed {
		t.Errorf("upgraded row must flip Managed=false so the console shows it as detected/native, not compose-managed")
	}
	if len(upgraded.Models) != 1 || upgraded.Models[0] != "llama3" {
		t.Errorf("upgraded row models = %v, want [llama3]", upgraded.Models)
	}
}

// TestMergeDetectedEnginesEmptyManagedStillReportsDetected covers the
// exact bug report: zero manifest services, one engine running outside
// citadel — the pane must not read as empty.
func TestMergeDetectedEnginesEmptyManagedStillReportsDetected(t *testing.T) {
	got := mergeDetectedEngines(nil, []status.LocalEngine{
		{Name: "ollama", Port: 11434, Models: []string{"llama3"}},
	})

	if len(got) != 1 {
		t.Fatalf("expected 1 detected row, got %d: %+v", len(got), got)
	}
	if got[0].Managed {
		t.Errorf("expected Managed=false, got true")
	}
}

// TestMergeDetectedEnginesNoDiscoveredEngines pins that an empty discovery
// result leaves the managed rows byte-for-byte unchanged (no spurious rows,
// no mutation of the input slice's contents).
func TestMergeDetectedEnginesNoDiscoveredEngines(t *testing.T) {
	managed := []controlcenter.ServiceInfo{
		{Name: "vllm", Status: "stopped", Managed: true},
	}

	got := mergeDetectedEngines(managed, nil)

	if len(got) != 1 {
		t.Fatalf("expected 1 row (unchanged), got %d: %+v", len(got), got)
	}
	if got[0].Name != managed[0].Name || got[0].Status != managed[0].Status || got[0].Managed != managed[0].Managed {
		t.Errorf("managed row was mutated: got %+v, want %+v", got[0], managed[0])
	}
}

// TestMergeDetectedEnginesDedupesDuplicateDiscovered guards against a
// hypothetical future discovery source returning the same engine name twice
// (e.g. a native process AND a container both answering) — only one detected
// row should result.
func TestMergeDetectedEnginesDedupesDuplicateDiscovered(t *testing.T) {
	got := mergeDetectedEngines(nil, []status.LocalEngine{
		{Name: "ollama", Port: 11434, Models: []string{"llama3"}},
		{Name: "ollama", Port: 11434, Models: []string{"llama3"}},
	})

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 detected row for a duplicate name, got %d: %+v", len(got), got)
	}
}
