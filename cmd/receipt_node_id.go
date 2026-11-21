package cmd

// servingReceiptNodeID is the node_id a regular worker signs into its receipts.
// The platform verifier requires it to equal the serving node id it recorded
// for the job, which for runWork is the nodeName published through
// setNodeMeta, the heartbeat and usage records (citadel-cli#1638). It is the
// same value, untrimmed, so the two can never disagree. A fabric node id echoed
// later (aceteam #8139) is deliberately not preferred: the verifier compares
// against the serving name, not the numeric id. An empty name leaves "" so
// aep.ResolveNodeID falls back to the key fingerprint. HPC workers sign their
// enrolled node id through their own resolver.
func servingReceiptNodeID(nodeName string) string { return nodeName }
