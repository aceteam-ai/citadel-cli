package cmd

import "testing"

// TestNodeWakeChannel pins the wire literal for the push-based dispatch wake
// (issue #7270). This string MUST match the backend's
// utils.queue_names.build_node_wake_channel; the two repos cannot share a
// constant, so each pins the literal in a test.
func TestNodeWakeChannel(t *testing.T) {
	cases := map[string]string{
		"1297":    "wake:v1:node:1297",
		"my-node": "wake:v1:node:my-node",
	}
	for nodeID, want := range cases {
		if got := nodeWakeChannel(nodeID); got != want {
			t.Errorf("nodeWakeChannel(%q) = %q, want %q", nodeID, got, want)
		}
	}
}

// TestNodeWakeChannelTracksNodeQueue guards that the wake channel and the
// per-node stream stay keyed by the SAME node id (a nudge must address the same
// node whose stream it wakes).
func TestNodeWakeChannelTracksNodeQueue(t *testing.T) {
	const org, node = "org-abc", "1297"
	q := nodeQueueName(org, node)
	w := nodeWakeChannel(node)
	if want := "jobs:v1:shell:org_org-abc:node:1297"; q != want {
		t.Fatalf("nodeQueueName = %q, want %q", q, want)
	}
	if want := "wake:v1:node:1297"; w != want {
		t.Fatalf("nodeWakeChannel = %q, want %q", w, want)
	}
}
