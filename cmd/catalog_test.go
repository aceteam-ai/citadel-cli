// cmd/catalog_test.go
package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
)

// TestRecordCatalogModuleLockRecognizedByListInstalled is the direct
// regression test for the citadel#739 follow-up (root-gap fix): before this,
// runCatalogInstall (reached by both `citadel catalog install <name>` and
// `citadel module install <catalog-name>`) never wrote a lockfile entry, so a
// catalog-CLI-installed module was invisible to the lockfile-scoped
// liveModuleOps.ListInstalled introduced by #739. This pins the closed loop:
// after recordCatalogModuleLock runs (as runCatalogInstall now does), the
// SAME module is recognized as installed.
func TestRecordCatalogModuleLockRecognizedByListInstalled(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})

	// Mirrors what runCatalogInstall now does after a successful catalog install.
	recordCatalogModuleLock("vllm", map[string]string{"PORT": "8000"}, false, "")

	o := newTestModuleOps(map[string]bool{"vllm": true})
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}
	if len(actual) != 1 || actual[0].Name != "vllm" || actual[0].Source != "vllm" {
		t.Fatalf("want vllm recognized as installed via its lockfile entry, got %+v", actual)
	}
	if actual[0].Config["PORT"] != "8000" {
		t.Errorf("config not recorded: %+v", actual[0].Config)
	}
}

// TestCatalogInstalledModuleNoLongerReinstallsOnRetarget is the end-to-end
// proof: a MODULE_SET-style desired assignment naming a catalog-CLI-installed
// module by its bare catalog name now converges as a no-op (through the REAL
// reconcile.Reconcile), instead of planning an uninstall+reinstall the way an
// unrecorded (e.g. embedded/operator-run) service still does -- see
// TestDesiredNamingUnmanagedEmbeddedServiceReinstallsRatherThanAdopts for that
// contrasting, still-accepted case.
func TestCatalogInstalledModuleNoLongerReinstallsOnRetarget(t *testing.T) {
	writeManifestWithServices(t, []Service{
		{Name: "vllm", ComposeFile: filepath.Join("services", "vllm.yml")},
	})
	recordCatalogModuleLock("vllm", nil, false, "")

	o := newTestModuleOps(map[string]bool{"vllm": true})
	actual, err := o.ListInstalled(context.Background())
	if err != nil {
		t.Fatalf("ListInstalled: %v", err)
	}

	desired := reconcile.DesiredState{
		Revision: "rev-1",
		Modules:  []reconcile.ModuleAssignment{{Name: "vllm", Source: "vllm"}},
	}
	plan, err := reconcile.Reconcile(context.Background(), desired, actual)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !plan.IsEmpty() {
		t.Fatalf("want an empty (already-converged, no reinstall) plan, got %+v", plan.Steps)
	}
}

// Ensure the catalog.LockEntry shape recordCatalogModuleLock writes stays
// minimal and intentional (no stray ref/commit/images for a catalog source).
func TestRecordCatalogModuleLockEntryShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	recordCatalogModuleLock("ollama", map[string]string{"K": "V"}, true, "")

	lf, err := catalog.LoadLockfile()
	if err != nil {
		t.Fatalf("LoadLockfile: %v", err)
	}
	e, ok := lf.LookupLock("ollama")
	if !ok {
		t.Fatal("expected a lockfile entry for ollama")
	}
	if e.Source != "ollama" || e.Ref != "" || e.Commit != "" || len(e.Images) != 0 {
		t.Errorf("unexpected entry shape: %+v", e)
	}
	// citadel#624 D1: the operator/catalog CLI path must leave ManagedBy EMPTY
	// (unstamped = protected from drift-uninstall). Only the desired-state
	// converge path (liveModuleOps.recordLock) stamps it.
	if e.ManagedBy != "" {
		t.Errorf("catalog-CLI install must be UNSTAMPED (protected), got ManagedBy=%q", e.ManagedBy)
	}
	if !e.Sandboxed {
		t.Errorf("want Sandboxed=true carried through")
	}
	if e.Config["K"] != "V" {
		t.Errorf("config not carried through: %+v", e.Config)
	}
}
