package cmd

import (
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
)

// TestRefuseBridgeProvisionUnderNodeDir pins the citadel#624 FIX A hardening: the
// bespoke bridge provision refuses under an active CITADEL_NODE_DIR override
// (where the delegation signal and compose project both resolve unsafely and a
// bespoke deploy could compose over the real node's bridge), and is a byte-clean
// no-op when no override is active.
func TestRefuseBridgeProvisionUnderNodeDir(t *testing.T) {
	if err := refuseBridgeProvisionUnderNodeDir(); err != nil {
		t.Fatalf("no override active must be a no-op, got: %v", err)
	}

	t.Setenv("CITADEL_NODE_DIR", "/tmp/override-node")
	err := refuseBridgeProvisionUnderNodeDir()
	if err == nil {
		t.Fatal("an active override must refuse the bespoke bridge provision")
	}
	if !strings.Contains(err.Error(), "/tmp/override-node") {
		t.Errorf("refusal should name the active override, got: %v", err)
	}
}

// TestUpdatedLockEntryPreservesProvenance pins citadel#624 FIX C: `citadel module
// update` rebuilds the lock entry, and must PRESERVE the desired-state provenance
// stamp (ManagedBy) and the health source (HealthComposeService) from the prior
// entry -- dropping ManagedBy would silently un-manage a desired-state module (so
// a later reconcile stops converging it), and dropping HealthComposeService would
// reset the bridge's health source and reintroduce the perpetual-ActionStart bug.
// It still REFRESHES the resolved ref/commit/images/sandbox.
func TestUpdatedLockEntryPreservesProvenance(t *testing.T) {
	prev := catalog.LockEntry{
		Name:                 "whatsapp-bridge",
		Source:               "sunapi386/whatsapp-bridge",
		Ref:                  "main",
		ResolvedRef:          "v1.0.0",
		Commit:               "oldcommit",
		Sandboxed:            false,
		ManagedBy:            reconcile.ManagedByDesiredState,
		HealthComposeService: "bridge",
	}
	newImages := []catalog.LockImage{{Ref: "ghcr.io/x:latest", Digest: "sha256:new"}}

	got := updatedLockEntry(prev, "whatsapp-bridge", "v1.1.0", "newcommit", newImages, true)

	// Preserved provenance + health source.
	if got.ManagedBy != reconcile.ManagedByDesiredState {
		t.Errorf("ManagedBy = %q, want it preserved as %q (citadel#624 FIX C)", got.ManagedBy, reconcile.ManagedByDesiredState)
	}
	if got.HealthComposeService != "bridge" {
		t.Errorf("HealthComposeService = %q, want it preserved as %q", got.HealthComposeService, "bridge")
	}
	// Preserved source identity.
	if got.Source != prev.Source || got.Ref != prev.Ref {
		t.Errorf("source identity changed: %q@%q, want %q@%q", got.Source, got.Ref, prev.Source, prev.Ref)
	}
	// Refreshed resolved bits.
	if got.ResolvedRef != "v1.1.0" || got.Commit != "newcommit" || !got.Sandboxed {
		t.Errorf("resolved bits not refreshed: %+v", got)
	}
	if len(got.Images) != 1 || got.Images[0].Digest != "sha256:new" {
		t.Errorf("images not refreshed: %+v", got.Images)
	}
}

// TestUpdatedLockEntryUnstampedStaysUnstamped: an operator/catalog-installed
// (UNSTAMPED) module must NOT gain a desired-state stamp merely by being updated
// -- update preserves whatever provenance the entry had, empty included.
func TestUpdatedLockEntryUnstampedStaysUnstamped(t *testing.T) {
	prev := catalog.LockEntry{Name: "vllm", Source: "vllm"} // ManagedBy empty
	got := updatedLockEntry(prev, "vllm", "", "c2", nil, false)
	if got.ManagedBy != "" {
		t.Errorf("an unstamped module must stay unstamped across update, got ManagedBy=%q", got.ManagedBy)
	}
}
