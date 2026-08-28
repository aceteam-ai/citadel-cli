// cmd/service_ram_override_test.go
//
// Pins the citadel#831 boot-path scope decision: composeFileArgs must NEVER
// pick up a `<name>.ram.yml` RAM-ceiling override, even when one exists on
// disk from a prior job-driven run. A file left over from an earlier
// CITADEL_RESOURCE_ISOLATION=1 run has no way to be re-checked against the
// current flag state from this boot-time path (it has no per-call access to
// the gate internal/jobs.applyRAMIsolation uses), so applying it here would
// silently defeat an operator's opt-out. See composeFileArgs' doc comment
// (cmd/service.go) for the full reasoning.
package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComposeFileArgs_NeverAppliesGPURAMOverride(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "media.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  media:\n    image: x\n"), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	// Simulate a RAM ceiling override left behind by an earlier job-driven
	// SERVICE_START with resource isolation enabled.
	ramOverridePath := filepath.Join(dir, "media.ram.yml")
	if err := os.WriteFile(ramOverridePath, []byte("services:\n  media:\n    mem_limit: 1b\n"), 0o600); err != nil {
		t.Fatalf("write ram override: %v", err)
	}

	args := composeFileArgs(composePath, composePath)
	for i, a := range args {
		if a == ramOverridePath {
			t.Fatalf("composeFileArgs must never apply the RAM ceiling override (found at args[%d]): %v", i, args)
		}
	}
}
