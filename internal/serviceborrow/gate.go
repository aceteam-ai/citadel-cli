package serviceborrow

import (
	"context"
	"fmt"
)

// PeerIdentity is the verified mesh identity of a connecting peer, mirroring
// network.PeerIdentity / gateway.MeshPeerIdentity field-for-field. It is a local
// copy (not a shared type) so this package need not import internal/network; the
// cmd layer copies the fields from network.WhoIsPeer's result into the resolver.
//
// StableID and OwnerID are the rename- and IP-reuse-stable trust keys (#1068 /
// aceteam#8553 S3.0): an EMPTY value means "unverified for that dimension" and
// makes the gate fail closed. NodeName/LoginName are display-oriented (they
// change on rename); only LoginName is a policy input, and only against the
// operator-owned platform allowlist.
type PeerIdentity struct {
	NodeName  string
	LoginName string
	StableID  string
	OwnerID   string
	SameOwner bool
}

// IdentityResolver resolves an inbound connection's remote address to a verified
// mesh peer identity. Injected so this package stays standalone and
// adversarially testable without a live mesh — mirrors gateway.MeshIdentityResolver
// / egressrelay.IdentityResolver exactly. Production wires network.WhoIsPeer
// through the cmd layer; a resolver error MUST be treated as unverified (the gate
// fails closed).
type IdentityResolver interface {
	// ResolvePeer resolves remoteAddr ("ip" or "ip:port") to a verified identity,
	// or an error if the peer cannot be verified.
	ResolvePeer(ctx context.Context, remoteAddr string) (*PeerIdentity, error)
}

// MockIdentityResolver is an IdentityResolver for tests. It returns a fixed
// identity (or error) and RECORDS the remote address it was asked about, so a
// test can assert the gate keyed authorization on the verified mesh RemoteAddr
// (not a spoofable header).
type MockIdentityResolver struct {
	Identity *PeerIdentity
	Err      error
	// LastAddr is set to the most recent remoteAddr passed to ResolvePeer.
	LastAddr string
}

// ResolvePeer implements IdentityResolver for the mock.
func (m *MockIdentityResolver) ResolvePeer(_ context.Context, remoteAddr string) (*PeerIdentity, error) {
	m.LastAddr = remoteAddr
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Identity, nil
}

// Decide is the PURE authorization decision — the heart of the serving gate. It
// returns nil when the peer may borrow, or a typed *AuthzError when it may not.
//
// The rule, in order (fail CLOSED at every ambiguity):
//
//  1. A nil identity is unverified (the resolver produced nothing to trust).
//  2. An identity with an empty StableID or empty OwnerID is unverified — the
//     coordination server did not vouch a stable device/owner id for it (#1068).
//     This check runs BEFORE the allowlist, so an allowlisted login can NEVER
//     bypass mesh verification.
//  3. A verified identity is authorized iff it is SameOwner OR its LoginName is
//     on the platform infra allowlist. Anything else — a shared-org or foreign
//     peer — is refused as not_authorized.
//
// allowed may be nil (or empty), which reduces to "same-owner only" — the exact
// egress-relay posture (internal/egressrelay.Authorize). The non-empty case adds
// the platform infra identity, mirroring cmd/platform_exposure.go's
// `sameOwner := id.SameOwner && id.OwnerID != ""; if !sameOwner && !allowed[login]`.
func Decide(id *PeerIdentity, allowed map[string]bool) error {
	if id == nil {
		return &AuthzError{Reason: ReasonUnverified, Detail: "resolver returned no identity"}
	}
	if id.StableID == "" || id.OwnerID == "" {
		return &AuthzError{Reason: ReasonUnverified, Detail: "peer has no verified stable/owner id"}
	}
	sameOwner := id.SameOwner && id.OwnerID != ""
	if sameOwner || allowed[id.LoginName] {
		return nil
	}
	return &AuthzError{Reason: ReasonNotAuthorized, Detail: fmt.Sprintf("peer %q (%s) is not a same-owner or platform-infra peer", id.NodeName, id.LoginName)}
}

// Authorize resolves remoteAddr to a verified identity via resolver, then applies
// Decide. On success it returns the verified identity (for the caller's audit
// log). On any failure it returns a typed *AuthzError — a nil resolver, a
// resolver error, a nil identity, and a policy refusal are ALL fail-closed. The
// resolver is given remoteAddr verbatim, so the decision is bound to the verified
// mesh source address and never to a request header.
func Authorize(ctx context.Context, resolver IdentityResolver, remoteAddr string, allowed map[string]bool) (*PeerIdentity, error) {
	if resolver == nil {
		return nil, &AuthzError{Reason: ReasonUnverified, Detail: "no identity resolver configured"}
	}
	id, err := resolver.ResolvePeer(ctx, remoteAddr)
	if err != nil {
		return nil, &AuthzError{Reason: ReasonUnverified, Detail: fmt.Sprintf("peer %s could not be verified: %v", remoteAddr, err)}
	}
	if derr := Decide(id, allowed); derr != nil {
		return nil, derr
	}
	return id, nil
}
