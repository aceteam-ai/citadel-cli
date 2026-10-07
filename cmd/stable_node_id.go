package cmd

import (
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nodeidentity"
)

// resolveStableNodeID returns this node's durable, machine-convergent identity
// fingerprint (the nodeidentity SPKI fingerprint, "sha256:<hex>") for the
// heartbeat, so the backend can map a stable id to the current (churn-prone)
// Headscale node id and rebind capabilities bound to it (#1235, part 3).
//
// It LOADS the key via Store.PublicKey() and never mints one: minting from the
// heartbeat could create a key a later `citadel init`/enroll would displace
// (the K-A no-side-effect-mint rule), so the reported id must not change as a
// side effect of status publishing. Returns "" when no node identity key exists
// yet; the heartbeat field is omitempty and inert until the aceteam backend
// consumes it.
func resolveStableNodeID() string {
	pub, err := nodeidentity.Convergent(network.GetNodeConfigDir()).PublicKey()
	if err != nil {
		return ""
	}
	fp, err := nodeidentity.FingerprintPublicKey(pub)
	if err != nil {
		return ""
	}
	return fp
}
