package serviceborrow

import (
	"context"
	"errors"
	"testing"
)

// sameOwnerVerified is a fully verified, same-owner peer — the one case the gate
// must ALLOW.
func sameOwnerVerified() *PeerIdentity {
	return &PeerIdentity{
		NodeName:  "peer-gpu-1",
		LoginName: "owner@example.com",
		StableID:  "nodekey:abc123",
		OwnerID:   "userid:42",
		SameOwner: true,
	}
}

func asAuthzError(t *testing.T, err error) *AuthzError {
	t.Helper()
	var ae *AuthzError
	if !errors.As(err, &ae) {
		t.Fatalf("want *AuthzError, got %T: %v", err, err)
	}
	return ae
}

// --- Decide: the authorization boundary (adversarial) ---------------------

func TestDecide_SameOwnerVerified_Allowed(t *testing.T) {
	if err := Decide(sameOwnerVerified(), nil); err != nil {
		t.Fatalf("same-owner verified peer must be allowed, got: %v", err)
	}
}

func TestDecide_SharedOrgPeer_Refused(t *testing.T) {
	// A verified peer in a DIFFERENT owner's tailnet, shared in (SameOwner false
	// but both ids present): a shared-org node is refused.
	id := sameOwnerVerified()
	id.SameOwner = false
	err := Decide(id, nil)
	if err == nil {
		t.Fatal("shared-org peer must be refused")
	}
	if ae := asAuthzError(t, err); !ae.IsNotAuthorized() {
		t.Fatalf("shared-org refusal reason = %q, want %q", ae.Reason, ReasonNotAuthorized)
	}
}

func TestDecide_ForeignOwnerPeer_Refused(t *testing.T) {
	id := &PeerIdentity{
		NodeName:  "stranger",
		LoginName: "attacker@evil.example",
		StableID:  "nodekey:zzz",
		OwnerID:   "userid:999", // a different owner
		SameOwner: false,
	}
	err := Decide(id, nil)
	if err == nil {
		t.Fatal("foreign-owner peer must be refused")
	}
	if ae := asAuthzError(t, err); !ae.IsNotAuthorized() {
		t.Fatalf("foreign refusal reason = %q, want %q", ae.Reason, ReasonNotAuthorized)
	}
}

func TestDecide_EmptyStableID_RefusedFailClosed(t *testing.T) {
	id := sameOwnerVerified()
	id.StableID = "" // control plane did not vouch a stable device id
	err := Decide(id, nil)
	if err == nil {
		t.Fatal("peer with empty StableID must be refused (fail closed)")
	}
	if ae := asAuthzError(t, err); !ae.IsUnverified() {
		t.Fatalf("empty-StableID reason = %q, want %q", ae.Reason, ReasonUnverified)
	}
}

func TestDecide_EmptyOwnerID_RefusedFailClosed(t *testing.T) {
	id := sameOwnerVerified()
	id.OwnerID = ""
	err := Decide(id, nil)
	if err == nil {
		t.Fatal("peer with empty OwnerID must be refused (fail closed)")
	}
	if ae := asAuthzError(t, err); !ae.IsUnverified() {
		t.Fatalf("empty-OwnerID reason = %q, want %q", ae.Reason, ReasonUnverified)
	}
}

func TestDecide_NilIdentity_RefusedFailClosed(t *testing.T) {
	err := Decide(nil, nil)
	if err == nil {
		t.Fatal("nil identity must be refused")
	}
	if ae := asAuthzError(t, err); !ae.IsUnverified() {
		t.Fatalf("nil-identity reason = %q, want %q", ae.Reason, ReasonUnverified)
	}
}

func TestDecide_PlatformInfraAllowlistLogin_Allowed(t *testing.T) {
	// A verified peer that is NOT same-owner but whose login is on the platform
	// infra allowlist is allowed (the "OR the platform infra identity" branch).
	id := &PeerIdentity{
		NodeName:  "platform-coordinator",
		LoginName: "infra@aceteam.ai",
		StableID:  "nodekey:infra",
		OwnerID:   "userid:1",
		SameOwner: false,
	}
	allowed := map[string]bool{"infra@aceteam.ai": true}
	if err := Decide(id, allowed); err != nil {
		t.Fatalf("platform infra login must be allowed, got: %v", err)
	}
}

func TestDecide_AllowlistDoesNotBypassVerification(t *testing.T) {
	// A login on the allowlist but with an EMPTY stable/owner id is still refused:
	// the allowlist can never bypass mesh verification (fail closed).
	id := &PeerIdentity{
		NodeName:  "spoofed-infra",
		LoginName: "infra@aceteam.ai",
		StableID:  "", // unverified
		OwnerID:   "",
		SameOwner: false,
	}
	allowed := map[string]bool{"infra@aceteam.ai": true}
	err := Decide(id, allowed)
	if err == nil {
		t.Fatal("allowlisted login with unverified identity must still be refused")
	}
	if ae := asAuthzError(t, err); !ae.IsUnverified() {
		t.Fatalf("reason = %q, want %q (allowlist must not bypass verification)", ae.Reason, ReasonUnverified)
	}
}

func TestDecide_NonAllowlistedForeignLogin_Refused(t *testing.T) {
	id := &PeerIdentity{
		NodeName:  "stranger",
		LoginName: "someone@else.example",
		StableID:  "nodekey:x",
		OwnerID:   "userid:7",
		SameOwner: false,
	}
	allowed := map[string]bool{"infra@aceteam.ai": true}
	if err := Decide(id, allowed); err == nil {
		t.Fatal("a foreign login not on the allowlist must be refused even when an allowlist exists")
	}
}

// --- Authorize: resolver wiring + fail-closed + RemoteAddr binding --------

func TestAuthorize_NilResolver_FailsClosed(t *testing.T) {
	_, err := Authorize(context.Background(), nil, "100.64.0.9:5000", nil)
	if err == nil {
		t.Fatal("nil resolver must fail closed")
	}
	if ae := asAuthzError(t, err); !ae.IsUnverified() {
		t.Fatalf("reason = %q, want %q", ae.Reason, ReasonUnverified)
	}
}

func TestAuthorize_ResolverError_FailsClosed(t *testing.T) {
	r := &MockIdentityResolver{Err: errors.New("whois failed")}
	_, err := Authorize(context.Background(), r, "100.64.0.9:5000", nil)
	if err == nil {
		t.Fatal("resolver error must fail closed")
	}
	if ae := asAuthzError(t, err); !ae.IsUnverified() {
		t.Fatalf("reason = %q, want %q", ae.Reason, ReasonUnverified)
	}
}

func TestAuthorize_SuccessReturnsIdentity(t *testing.T) {
	want := sameOwnerVerified()
	r := &MockIdentityResolver{Identity: want}
	got, err := Authorize(context.Background(), r, "100.64.0.9:5000", nil)
	if err != nil {
		t.Fatalf("same-owner verified must authorize, got: %v", err)
	}
	if got != want {
		t.Fatalf("Authorize returned %+v, want the resolved identity", got)
	}
}

// TestAuthorize_KeysOnRemoteAddrNotHeader asserts the gate binds its decision to
// the address passed in (the request's verified mesh RemoteAddr), never a header.
// A spoofed X-Forwarded-For has no way to reach the resolver because Authorize
// forwards ONLY the remoteAddr argument.
func TestAuthorize_KeysOnRemoteAddrNotHeader(t *testing.T) {
	r := &MockIdentityResolver{Identity: sameOwnerVerified()}
	const remoteAddr = "100.64.0.9:41000"
	if _, err := Authorize(context.Background(), r, remoteAddr, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.LastAddr != remoteAddr {
		t.Fatalf("resolver was asked about %q, want the verified RemoteAddr %q", r.LastAddr, remoteAddr)
	}
}
