package controlcenter

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
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

// TestNextChatPaneIndex covers the edge-bubble navigation: Tab cycles panes in
// the MIDDLE of the list but, instead of wrapping, returns an out-of-range index
// at the edges (count forward-past-last, -1 backward-before-first) so the caller
// bubbles the key up and the user leaves the Chat tab. It also covers the
// "focus not on a chat pane yet" start case (which enters the list, never
// bubbles) and the empty-panes guard.
func TestNextChatPaneIndex(t *testing.T) {
	const count = 4 // panes: input, messages, peers, channels
	tests := []struct {
		name           string
		focused, delta int
		want           int
	}{
		// Middle-of-list cycling (focus moves, stays in range).
		{"forward from input to messages", 0, 1, 1},
		{"forward from messages to peers", 1, 1, 2},
		{"forward from peers to channels", 2, 1, 3},
		{"backward from channels to peers", 3, -1, 2},
		{"backward from messages to input", 1, -1, 0},
		// Edge bubbles: out-of-range signals "leave the tab".
		{"forward at last pane bubbles (== count)", count - 1, 1, count},
		{"backward at first pane bubbles (== -1)", 0, -1, -1},
		// Focus not yet on a chat pane: enter the list, do NOT bubble.
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

// TestNextChatPaneIndex_EdgeResultsAreBubbles asserts the contract the caller
// relies on: at a pane edge the returned index is out of [0, count), which
// cycleChatPane treats as "past the edge -> bubble" (and, critically, guards so
// the positive forward sentinel `count` never indexes panes[count] and panics).
func TestNextChatPaneIndex_EdgeResultsAreBubbles(t *testing.T) {
	const count = 4
	if idx := nextChatPaneIndex(count-1, count, 1); idx >= 0 && idx < count {
		t.Errorf("forward-at-last idx = %d, want out of [0,%d) to bubble", idx, count)
	}
	if idx := nextChatPaneIndex(0, count, -1); idx >= 0 && idx < count {
		t.Errorf("backward-at-first idx = %d, want out of [0,%d) to bubble", idx, count)
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

// TestWriteStatus_DoesNotTouchAppQueue is the deadlock regression. writeStatus
// is the DIRECT status writer meant to run on the main goroutine (inside an
// existing QueueUpdateDraw callback), so it must render straight into msgView
// WITHOUT calling p.app.QueueUpdateDraw. We construct the page with a real
// msgView but a nil p.app: if writeStatus ever re-introduces QueueUpdateDraw
// (which is exactly the nested-QueueUpdateDraw bug that froze the whole Control
// Center on connect), it will dereference the nil app and panic here. A passing
// run therefore mechanically forbids the regression — it proves writeStatus
// never touches the app queue.
func TestWriteStatus_DoesNotTouchAppQueue(t *testing.T) {
	p := &ChatPage{msgView: tview.NewTextView()}
	// p.app is deliberately nil: any QueueUpdateDraw path would panic.

	p.writeStatus("hello-status-marker")

	if got := p.msgView.GetText(true); !strings.Contains(got, "hello-status-marker") {
		t.Errorf("writeStatus did not render into msgView: got %q", got)
	}
}

// TestWriteStatus_NilMsgViewIsSafe documents that writeStatus is a no-op when
// the view has not been built yet, mirroring renderStatusBar's nil guard, so a
// status write racing page teardown never panics.
func TestWriteStatus_NilMsgViewIsSafe(t *testing.T) {
	p := &ChatPage{}
	p.writeStatus("anything") // must not panic
}
