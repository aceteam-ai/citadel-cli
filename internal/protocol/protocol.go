// Package protocol declares the Citadel ↔ AceTeam fabric wire-contract version.
package protocol

// FabricProtocolVersion is the version of the node↔control-plane wire contract:
// job payloads, the reconcile desired/actual state shape, MCP tool shapes, and
// the nexus registration handshake. It is a single integer bumped ONLY on a
// breaking wire change.
//
// It is deliberately decoupled from the product/release version (vX.Y.Z): the
// node agent (citadel-cli, git-tag + GitHub release) and the control plane
// (aceteam, main→production + Railway) release on independent cadences, so a
// product release must never force a counterpart release. The protocol integer
// is the contract that lets them refuse to drift apart silently. The aceteam
// repo declares a matching constant. See aceteam-ai/citadel-cli#363.
//
// Phase 1 (current): declared, surfaced in `citadel version`, and stamped into
// release notes — no runtime negotiation/enforcement yet. Phase 2 (advertise in
// the registration handshake, negotiate, surface incompatibility in /fabric)
// rides the remote-managed-nodes epic (#352).
const FabricProtocolVersion = 1
