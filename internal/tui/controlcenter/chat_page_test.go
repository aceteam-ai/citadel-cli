package controlcenter

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestClassifyChatKey_ReleasesNavigationKeys is the keyboard-trap regression: the
// chat input must only consume printable text + Enter (send) + scroll keys, and
// return everything navigational so the user can leave the Chat tab. Enter is
// intentionally passthrough here because sending is wired via the InputField's
// SetDoneFunc, not HandleInput.
func TestClassifyChatKey_ReleasesNavigationKeys(t *testing.T) {
	tests := []struct {
		name  string
		event *tcell.EventKey
		want  chatKeyAction
	}{
		{"escape defocuses", tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone), chatKeyDefocus},
		{"tab switches pane", tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone), chatKeyNextPane},
		{"shift+tab switches pane back", tcell.NewEventKey(tcell.KeyBacktab, 0, tcell.ModNone), chatKeyPrevPane},
		{"pgup scrolls", tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone), chatKeyScrollUp},
		{"pgdn scrolls", tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone), chatKeyScrollDown},
		{"enter passes through to send", tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), chatKeyPassthrough},
		{"printable rune passes through", tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone), chatKeyPassthrough},
		{"space passes through", tcell.NewEventKey(tcell.KeyRune, ' ', tcell.ModNone), chatKeyPassthrough},
		// Alt+digit is a global tab accelerator handled upstream; it must never be
		// swallowed as text by the chat input.
		{"alt+3 passes through untouched", tcell.NewEventKey(tcell.KeyRune, '3', tcell.ModAlt), chatKeyPassthrough},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyChatKey(tt.event); got != tt.want {
				t.Errorf("classifyChatKey(%s) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

// TestNextChatPaneIndex covers forward/backward cycling and the "focus not on a
// chat pane yet" (-1) start case.
func TestNextChatPaneIndex(t *testing.T) {
	const count = 4
	tests := []struct {
		name           string
		focused, delta int
		want           int
	}{
		{"forward from input", 0, 1, 1},
		{"forward wraps to input", count - 1, 1, 0},
		{"backward from input wraps to last", 0, -1, count - 1},
		{"backward from second", 1, -1, 0},
		{"no focus forward starts at input", -1, 1, 0},
		{"no focus backward starts at last", -1, -1, count - 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextChatPaneIndex(tt.focused, count, tt.delta); got != tt.want {
				t.Errorf("nextChatPaneIndex(%d, %d, %d) = %d, want %d",
					tt.focused, count, tt.delta, got, tt.want)
			}
		})
	}
	if got := nextChatPaneIndex(0, 0, 1); got != -1 {
		t.Errorf("nextChatPaneIndex with zero panes = %d, want -1", got)
	}
}

// TestFormatChatStatusBar verifies the connection-state transitions render the
// right status text: connecting spinner, connected, and — the key regression —
// a real error state (never a permanent spinner) with the failure detail.
func TestFormatChatStatusBar(t *testing.T) {
	tests := []struct {
		name             string
		state            connState
		endpoint, detail string
		wantContains     []string
		wantMissing      []string
	}{
		{
			name:         "connecting shows spinner",
			state:        connConnecting,
			endpoint:     "wss://aceteam.ai",
			wantContains: []string{"connecting", "wss://aceteam.ai"},
		},
		{
			name:         "connected shows connected",
			state:        connConnected,
			endpoint:     "wss://aceteam.ai",
			wantContains: []string{"connected", "wss://aceteam.ai"},
			wantMissing:  []string{"connecting"},
		},
		{
			name:         "error surfaces detail, not a spinner",
			state:        connError,
			endpoint:     "wss://aceteam.ai",
			detail:       "timed out",
			wantContains: []string{"error", "timed out"},
			wantMissing:  []string{"connecting"},
		},
		{
			name:         "empty endpoint falls back to not configured",
			state:        connConnecting,
			endpoint:     "",
			wantContains: []string{"not configured"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatChatStatusBar(tt.state, tt.endpoint, tt.detail)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("formatChatStatusBar = %q, want it to contain %q", got, want)
				}
			}
			for _, missing := range tt.wantMissing {
				if strings.Contains(got, missing) {
					t.Errorf("formatChatStatusBar = %q, want it to NOT contain %q", got, missing)
				}
			}
		})
	}
}

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
