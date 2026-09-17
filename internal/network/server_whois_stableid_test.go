// internal/network/server_whois_stableid_test.go
// Pins that NetworkServer.WhoIs preserves the STABLE device id and OWNER id from
// the coordination server's WhoIs response (aceteam#8553 S3.0) — the rename- and
// IP-reuse-stable trust keys later cache-transfer delegation binds to — without
// weakening the existing SameOwner gate.
package network

import (
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

func TestWhoIs_PreservesStableAndOwnerIdentity(t *testing.T) {
	const selfUser = tailcfg.UserID(7)
	b := &fakeBackend{
		status: &ipnstate.Status{Self: &ipnstate.PeerStatus{UserID: selfUser}},
		whois: &apitype.WhoIsResponse{
			Node: &tailcfg.Node{
				StableID:     "nodekey-stable-abc123",
				User:         selfUser,
				ComputedName: "peer-node",
			},
			UserProfile: &tailcfg.UserProfile{ID: selfUser, LoginName: "owner@example.com"},
		},
	}

	id, err := serverWith(b).WhoIs(t.Context(), "100.64.0.9:1234")
	if err != nil {
		t.Fatalf("WhoIs() error = %v", err)
	}
	if id.StableID != "nodekey-stable-abc123" {
		t.Errorf("StableID = %q, want the WhoIs node's StableID", id.StableID)
	}
	if id.OwnerID != selfUser.String() {
		t.Errorf("OwnerID = %q, want %q", id.OwnerID, selfUser.String())
	}
	if id.NodeName != "peer-node" {
		t.Errorf("NodeName = %q, want peer-node", id.NodeName)
	}
	if id.LoginName != "owner@example.com" {
		t.Errorf("LoginName = %q, want owner@example.com", id.LoginName)
	}
	if !id.SameOwner {
		t.Error("SameOwner = false, want true (peer owned by self's user)")
	}
}

// A different-owner peer still CARRIES its stable/owner id (they describe the
// peer regardless), but SameOwner stays false so the existing gate is unchanged.
func TestWhoIs_DifferentOwnerStillCarriesIdentityButNotSameOwner(t *testing.T) {
	b := &fakeBackend{
		status: &ipnstate.Status{Self: &ipnstate.PeerStatus{UserID: tailcfg.UserID(7)}},
		whois: &apitype.WhoIsResponse{
			Node: &tailcfg.Node{
				StableID:     "nodekey-stable-other",
				User:         tailcfg.UserID(99),
				ComputedName: "stranger",
			},
			UserProfile: &tailcfg.UserProfile{ID: tailcfg.UserID(99), LoginName: "stranger@elsewhere.com"},
		},
	}

	id, err := serverWith(b).WhoIs(t.Context(), "100.64.0.50")
	if err != nil {
		t.Fatalf("WhoIs() error = %v", err)
	}
	if id.SameOwner {
		t.Error("SameOwner = true, want false for a different-owner peer")
	}
	if id.StableID != "nodekey-stable-other" || id.OwnerID != tailcfg.UserID(99).String() {
		t.Errorf("identity not carried: StableID=%q OwnerID=%q", id.StableID, id.OwnerID)
	}
}

// A shared-in node (Sharer set) is never same-owner even when User matches self.
func TestWhoIs_SharedInNodeIsNotSameOwner(t *testing.T) {
	const selfUser = tailcfg.UserID(7)
	b := &fakeBackend{
		status: &ipnstate.Status{Self: &ipnstate.PeerStatus{UserID: selfUser}},
		whois: &apitype.WhoIsResponse{
			Node: &tailcfg.Node{
				StableID:     "nodekey-stable-shared",
				User:         selfUser,
				Sharer:       tailcfg.UserID(42),
				ComputedName: "shared-in",
			},
			UserProfile: &tailcfg.UserProfile{ID: selfUser, LoginName: "owner@example.com"},
		},
	}

	id, err := serverWith(b).WhoIs(t.Context(), "100.64.0.60")
	if err != nil {
		t.Fatalf("WhoIs() error = %v", err)
	}
	if id.SameOwner {
		t.Error("SameOwner = true, want false for a shared-in node (Sharer set)")
	}
}

// Missing identity: a WhoIs node with no StableID and a zero owner leaves both
// stable trust keys empty (empty => unverified), never a fabricated value.
func TestWhoIs_MissingIdentityLeavesTrustKeysEmpty(t *testing.T) {
	b := &fakeBackend{
		status: &ipnstate.Status{Self: &ipnstate.PeerStatus{UserID: tailcfg.UserID(7)}},
		whois: &apitype.WhoIsResponse{
			Node:        &tailcfg.Node{ComputedName: "anon"},
			UserProfile: &tailcfg.UserProfile{},
		},
	}

	id, err := serverWith(b).WhoIs(t.Context(), "100.64.0.70")
	if err != nil {
		t.Fatalf("WhoIs() error = %v", err)
	}
	if id.StableID != "" {
		t.Errorf("StableID = %q, want empty for a node with no stable id", id.StableID)
	}
	if id.OwnerID != "" {
		t.Errorf("OwnerID = %q, want empty for a zero owner (never a misleading userid:0)", id.OwnerID)
	}
}
