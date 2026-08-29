// cmd/module_reservations_test.go
package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModuleReservationsListDisplaysActiveReservations exercises the real
// findAndReadManifest -> jobs.ServiceHandler.ActiveReservations path (a pure
// manifest read, no docker involved) end to end.
func TestModuleReservationsListDisplaysActiveReservations(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml"), DesiredStatus: "stopped", EvictedByJob: "exclusive:bonsai"},
		{Name: "bonsai", ComposeFile: filepath.Join("services", "bonsai.yml")},
	})

	out, err := captureStdout(func() error { return runModuleReservationsList(context.Background()) })
	if err != nil {
		t.Fatalf("runModuleReservationsList: %v", err)
	}
	if !strings.Contains(out, "exclusive:bonsai") {
		t.Errorf("output = %q, want it to list the active reservation's job id", out)
	}
	if !strings.Contains(out, "vllm") {
		t.Errorf("output = %q, want it to list the evicted service", out)
	}
}

func TestModuleReservationsListNoneActive(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})

	out, err := captureStdout(func() error { return runModuleReservationsList(context.Background()) })
	if err != nil {
		t.Fatalf("runModuleReservationsList: %v", err)
	}
	if !strings.Contains(out, "No active") {
		t.Errorf("output = %q, want a clear \"no active reservations\" message", out)
	}
}

// TestModuleReservationsReleaseDryRunDoesNotMutate pins the --dry-run
// contract (mirrors module_control.go's identical guarantee): a preview
// never touches the manifest, and never calls Release's real restart path
// (so this stays hermetic without any docker/native start seam).
func TestModuleReservationsReleaseDryRunDoesNotMutate(t *testing.T) {
	configDir := writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml"), DesiredStatus: "stopped", EvictedByJob: "exclusive:bonsai"},
	})

	reservationsDryRun = true
	t.Cleanup(func() { reservationsDryRun = false })

	out, err := captureStdout(func() error { return runModuleReservationsRelease(context.Background(), "exclusive:bonsai") })
	if err != nil {
		t.Fatalf("runModuleReservationsRelease --dry-run: %v", err)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "vllm") {
		t.Errorf("output = %q, want a dry-run preview naming vllm", out)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "citadel.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(data), "evicted_by_job: exclusive:bonsai") {
		t.Errorf("manifest was mutated by --dry-run: %s", data)
	}
}

// TestModuleReservationsReleaseNoActiveReservationIsANoOp pins the safe,
// hermetic no-op path: Release for a job id with NO tagged service never
// calls the restart primitive at all (nothing to iterate), so this proves
// the CLI wiring without needing any docker/native start seam.
func TestModuleReservationsReleaseNoActiveReservationIsANoOp(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})

	out, err := captureStdout(func() error { return runModuleReservationsRelease(context.Background(), "exclusive:nonexistent") })
	if err != nil {
		t.Fatalf("runModuleReservationsRelease: %v", err)
	}
	if !strings.Contains(out, "No services were tagged") {
		t.Errorf("output = %q, want a clear no-op message", out)
	}
}

// TestModuleReservationsReleaseExpectNodeMismatchRefuses mirrors
// TestRunModuleControl_ExpectNodeMismatchRefuses (module_control_test.go):
// a mismatched --expect-node refuses BEFORE --dry-run's preview, and the
// manifest is left untouched.
func TestModuleReservationsReleaseExpectNodeMismatchRefuses(t *testing.T) {
	configDir := writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml"), DesiredStatus: "stopped", EvictedByJob: "exclusive:bonsai"},
	})
	manifest, _, err := findAndReadManifest()
	if err != nil {
		t.Fatalf("findAndReadManifest: %v", err)
	}
	manifest.Node.Name = "the-real-production-node"
	if err := writeManifest(filepath.Join(configDir, "citadel.yaml"), manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	reservationsExpectNode = "some-isolated-test-node"
	reservationsDryRun = true // even in preview mode, a mismatch must still refuse
	t.Cleanup(func() { reservationsExpectNode = ""; reservationsDryRun = false })

	err = runModuleReservationsRelease(context.Background(), "exclusive:bonsai")
	if err == nil {
		t.Fatal("want a refusal error on --expect-node mismatch")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("error = %v, want it to say it is refusing", err)
	}

	data, err := os.ReadFile(filepath.Join(configDir, "citadel.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(data), "evicted_by_job: exclusive:bonsai") {
		t.Errorf("a refused --expect-node mismatch must not mutate the manifest: %s", data)
	}
}

// TestModuleReservationsRefusesUnderNodeDirFlagOverride pins the
// refuseIfReservationNodeDirUnsupported wiring end to end: with --node-dir
// set as a FLAG (not the env var), both list and release refuse rather than
// silently acting through internal/jobs' env-only override visibility gap.
func TestModuleReservationsRefusesUnderNodeDirFlagOverride(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})
	setNodeDirOverrideForTest(t, t.TempDir())

	if err := runModuleReservationsList(context.Background()); err == nil {
		t.Fatal("want a refusal for 'reservations list' under a --node-dir FLAG override")
	}
	if err := runModuleReservationsRelease(context.Background(), "exclusive:x"); err == nil {
		t.Fatal("want a refusal for 'reservations release' under a --node-dir FLAG override")
	}
}
