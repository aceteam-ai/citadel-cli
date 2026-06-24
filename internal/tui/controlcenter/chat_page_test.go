package controlcenter

import "testing"

// TestChatPageResolveConfig_UsesProviderWhenSnapshotIncomplete reproduces the
// device-auth wiring bug: the ChatPage is constructed before the node completes
// device authorization, so its startup snapshot is empty. resolveConfig must
// pick up the credentials the provider resolves later (mirroring how the
// terminal/desktop/worker pages re-resolve token+org at network-connect time),
// rather than staying stuck on the empty snapshot and reporting "device
// authorization required".
func TestChatPageResolveConfig_UsesProviderWhenSnapshotIncomplete(t *testing.T) {
	// Snapshot at construction is empty (node not yet device-authed) but a
	// provider can resolve the real credentials persisted after auth.
	resolved := ChatPageConfig{
		APIBaseURL: "https://aceteam.ai",
		APIToken:   "device_api_token_xyz",
		OrgID:      "org-123",
		NodeID:     "node-abc",
		NodeName:   "mini",
	}
	p := NewChatPage(ChatPageConfig{
		Provider: func() ChatPageConfig { return resolved },
	})

	apiBaseURL, apiToken, orgID, nodeID, nodeName := p.resolveConfig()

	if apiBaseURL != resolved.APIBaseURL {
		t.Errorf("apiBaseURL = %q, want %q", apiBaseURL, resolved.APIBaseURL)
	}
	if apiToken != resolved.APIToken {
		t.Errorf("apiToken = %q, want %q", apiToken, resolved.APIToken)
	}
	if orgID != resolved.OrgID {
		t.Errorf("orgID = %q, want %q", orgID, resolved.OrgID)
	}
	if nodeID != resolved.NodeID {
		t.Errorf("nodeID = %q, want %q", nodeID, resolved.NodeID)
	}
	if nodeName != resolved.NodeName {
		t.Errorf("nodeName = %q, want %q", nodeName, resolved.NodeName)
	}

	// The connect() guard must now pass.
	if apiBaseURL == "" || apiToken == "" || orgID == "" {
		t.Fatalf("guard still trips after provider resolution: base=%q token=%q org=%q",
			apiBaseURL, apiToken, orgID)
	}
}

// TestChatPageResolveConfig_KeepsCompleteSnapshot verifies that when the startup
// snapshot is already complete (node device-authed before launch — the common
// case), resolveConfig does NOT invoke the provider and preserves the snapshot.
func TestChatPageResolveConfig_KeepsCompleteSnapshot(t *testing.T) {
	providerCalled := false
	p := NewChatPage(ChatPageConfig{
		APIBaseURL: "https://aceteam.ai",
		APIToken:   "tok",
		OrgID:      "org-1",
		NodeID:     "n1",
		NodeName:   "name1",
		Provider: func() ChatPageConfig {
			providerCalled = true
			return ChatPageConfig{}
		},
	})

	base, tok, org, _, _ := p.resolveConfig()
	if providerCalled {
		t.Error("provider should not be called when snapshot is already complete")
	}
	if base != "https://aceteam.ai" || tok != "tok" || org != "org-1" {
		t.Errorf("snapshot not preserved: base=%q tok=%q org=%q", base, tok, org)
	}
}

// TestChatPageResolveConfig_PartialProviderDoesNotClobber verifies that a
// provider returning only some fields fills the missing ones without wiping a
// credential that was already present in the snapshot.
func TestChatPageResolveConfig_PartialProviderDoesNotClobber(t *testing.T) {
	p := NewChatPage(ChatPageConfig{
		// Token already known from a prior partial write; org/base missing.
		APIToken: "existing-token",
		Provider: func() ChatPageConfig {
			return ChatPageConfig{
				APIBaseURL: "https://aceteam.ai",
				OrgID:      "org-late",
				// APIToken intentionally empty in this resolution.
			}
		},
	})

	base, tok, org, _, _ := p.resolveConfig()
	if tok != "existing-token" {
		t.Errorf("existing token clobbered: got %q", tok)
	}
	if base != "https://aceteam.ai" || org != "org-late" {
		t.Errorf("missing fields not filled: base=%q org=%q", base, org)
	}
}

// TestChatPageResolveConfig_NoProviderNoSnapshot verifies the genuinely
// unconfigured case still reports empty (so connect() shows the auth-required
// status) and does not panic on a nil provider.
func TestChatPageResolveConfig_NoProviderNoSnapshot(t *testing.T) {
	p := NewChatPage(ChatPageConfig{})
	base, tok, org, _, _ := p.resolveConfig()
	if base != "" || tok != "" || org != "" {
		t.Errorf("expected empty config, got base=%q tok=%q org=%q", base, tok, org)
	}
}
