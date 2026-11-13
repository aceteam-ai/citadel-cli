package cmd

import (
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// receiptNodeID picks the node_id a signed receipt carries. The platform
// verifier binds it to the id the node serves under, so the order is: the
// platform-echoed fabric node id (aceteam #8139), else the worker's routing
// node id (the name its per-node stream and heartbeat use), else "" so the
// caller's aep.ResolveNodeID falls back to the key fingerprint (HPC and
// not-yet-named nodes). Resolved at the cmd layer; aep semantics are unchanged.
func receiptNodeID(fabricID, routingNodeID string) string {
	if id := strings.TrimSpace(fabricID); id != "" {
		return id
	}
	return strings.TrimSpace(routingNodeID)
}

// liveReceiptNodeIDResolver returns a resolver that re-reads the fabric node id
// on every call (it may be echoed after startup) and falls back to routingNodeID.
func liveReceiptNodeIDResolver(routingNodeID string) func() string {
	return func() string {
		return receiptNodeID(config.LoadDeviceCredsConverged().FabricNodeID, routingNodeID)
	}
}
