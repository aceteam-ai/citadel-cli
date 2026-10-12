// Package serviceborrow is the GENERIC peer-service borrowing contract
// (aceteam-ai/citadel-cli#1289, aceteam epic #10876). It generalizes #1270 (a
// small CPU node borrowing a GPU peer's embedding service over the mesh) into
// ONE reusable contract for ANY node service — embedding, transcription, OCR,
// TTS, rendering — with embedding and transcription as the first adopters.
//
// The contract has two halves, both in this package so one authorization
// boundary serves every service:
//
//   - The SERVING gate (Decide / Authorize): the peer gateway's authorization
//     boundary. A borrow request is served only to a SAME-OWNER verified mesh
//     peer OR the platform infra identity; a shared-org, foreign, or unverified
//     caller is refused. This mirrors the established egress-relay posture
//     (internal/egressrelay.Authorize -> network.WhoIsPeer, verified mesh-peer
//     identity, NO token/passcode fallback) and the gateway's own private/org
//     and cache-serving gates (cmd/platform_exposure.go, gateway/cache_serve.go),
//     TIGHTENED by #1068's "empty stable/owner id means unverified -> refuse"
//     rule (which egress-relay predates). See gate.go.
//
//   - The REQUESTER protocol (Client, PlanRoute, ServiceIdentity.Match): dial a
//     verified peer's gateway over the mesh, read the service identity the peer
//     advertises, refuse on a mismatch with a typed error, and fall back to the
//     local service ONLY when the local identity matches — otherwise a typed
//     service_unavailable error. NEVER central, NEVER hosted. See client.go and
//     route.go.
//
// # Standalone by design
//
// This package imports only the standard library (and the stdlib-only leaf
// internal/meshtransfer for the client's verified mesh transport). It NEVER
// imports internal/network: callers inject the IdentityResolver (production
// wraps network.WhoIsPeer through the cmd layer), the MeshGateway (production
// wraps meshtransfer + network.Dial), and the allowlist reader. This keeps the
// gate/identity/route logic pure and adversarially unit-testable with no live
// mesh — mirroring internal/mesh, internal/meshtransfer, internal/ingress, and
// internal/egressrelay exactly. The cross-context rule (CLAUDE.md #787): the
// node config dir is resolved in cmd and passed in; this package never calls
// network.GetNodeConfigDir().
//
// # Trust model (fail closed everywhere)
//
// The SERVING gate keys identity ONLY on the coordination server's verified
// mesh identity (network.PeerIdentity.StableID / OwnerID / SameOwner), resolved
// from the request's verified mesh RemoteAddr — never a request header, job
// payload, hostname, or IP. A spoofed X-Forwarded-For is therefore ignored. The
// REQUESTER never dials a peer it has not first verified as a same-owner mesh
// peer, and its transport (meshtransfer) pins the peer's certificate captured
// over the authenticated mesh connection, so no public DNS, arbitrary URL,
// proxy, or redirect can steer a borrow request off the intended peer.
package serviceborrow

import "strings"

// ServiceIdentity is the identity a peer advertises for one borrowable service
// and that a requester requires before it will borrow. It is the GENERIC
// identity every service shares; a service that needs more (embedding's dtype /
// dimension, #1270) layers its own checks on top of Match without changing this
// contract.
type ServiceIdentity struct {
	// Kind is the service kind ("embedding", "transcription", ...). Always
	// required: a requester never borrows a service of a different kind.
	Kind string `json:"kind"`
	// Model is the model or engine id the service serves (e.g.
	// "gte-multilingual-base"). Empty means "unspecified / not advertised".
	Model string `json:"model,omitempty"`
	// Version is the service/engine version. Empty means "unspecified".
	Version string `json:"version,omitempty"`
}

// Capacity is the advertised capacity a requester uses to pick among candidate
// peers under `auto` selection. All fields are advisory hints, never a trust
// input.
type Capacity struct {
	// Device is the compute device the service runs on ("cuda", "cpu", ...). A
	// requester borrowing GPU work prefers a "cuda" peer.
	Device string `json:"device,omitempty"`
	// QueueDepth is the number of jobs currently queued/in-flight for the
	// service. Lower is better (less loaded).
	QueueDepth int `json:"queue_depth"`
	// QueueMax is the service's max concurrent capacity (0 = unknown). A
	// requester avoids a peer whose queue is already full.
	QueueMax int `json:"queue_max,omitempty"`
}

// Advertisement is the JSON a peer gateway serves at GET /borrow/<kind>/identity
// and a requester reads before borrowing. Ready reports whether the underlying
// loopback service is actually up (a requester never borrows from a not-ready
// peer).
type Advertisement struct {
	Identity ServiceIdentity `json:"identity"`
	Capacity Capacity        `json:"capacity"`
	Ready    bool            `json:"ready"`
}

// Match reports whether the advertised identity (the receiver) satisfies what a
// requester wants. It returns nil on a match, or a typed *IdentityMismatchError
// naming the first differing field so a caller can refuse with a precise reason.
//
// Rule (the GENERIC default; a service may require more but not less):
//   - Kind must always match (case-insensitive); a requester never borrows a
//     service of a different kind.
//   - Model must match (case-insensitive) WHEN want.Model is non-empty. A
//     non-empty wanted model against an empty or different advertised model is a
//     mismatch (the peer cannot confirm it serves the required model).
//   - Version must match (exact) WHEN want.Version is non-empty. Versions are
//     opaque strings, so the comparison is exact, not case-folded.
//
// An empty wanted Model/Version is "no constraint on that field". #1270 adds the
// embedding-specific dtype/dimension checks on top of this; they are NOT here.
func (adv ServiceIdentity) Match(want ServiceIdentity) error {
	// An empty wanted kind is a caller bug: a requester must always name the
	// service kind it needs. Fail closed rather than let two empty kinds "match".
	if strings.TrimSpace(want.Kind) == "" {
		return &IdentityMismatchError{Field: "kind", Want: want.Kind, Got: adv.Kind}
	}
	if !strings.EqualFold(strings.TrimSpace(adv.Kind), strings.TrimSpace(want.Kind)) {
		return &IdentityMismatchError{Field: "kind", Want: want.Kind, Got: adv.Kind}
	}
	if w := strings.TrimSpace(want.Model); w != "" && !strings.EqualFold(strings.TrimSpace(adv.Model), w) {
		return &IdentityMismatchError{Field: "model", Want: want.Model, Got: adv.Model}
	}
	if w := strings.TrimSpace(want.Version); w != "" && strings.TrimSpace(adv.Version) != w {
		return &IdentityMismatchError{Field: "version", Want: want.Version, Got: adv.Version}
	}
	return nil
}
