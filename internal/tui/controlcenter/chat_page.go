package controlcenter

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/chat"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// presenceTimeout is how long since last heartbeat before a node is considered offline.
const presenceTimeout = 90 * time.Second

// connectTimeout bounds how long the Chat tab shows "connecting…" before the
// watchdog flips it to a real timeout error. It is longer than the underlying
// WebSocket HandshakeTimeout (10s) so a dial failure surfaces its own specific
// error first; the watchdog only catches the cases the dial itself can't (e.g. a
// server that accepts the socket then closes on a scope mismatch, or a silently
// rejected subscribe that never yields an OnConnect).
const connectTimeout = 15 * time.Second

// connState describes the chat transport connection lifecycle.
type connState int

const (
	connConnecting connState = iota
	connConnected
	connError
)

// ChatPage implements the Page interface for the Chat tab.
// It provides org-scoped node-to-node messaging over the Redis API proxy.
type ChatPage struct {
	app *tview.Application

	// Config. The five fields below are seeded at construction from the initial
	// snapshot, but are NOT necessarily final: a node that completes device
	// authorization *after* the control center starts (the in-TUI device-auth
	// flow, or a re-pair) writes its credentials to disk only then. To match the
	// terminal/desktop/worker pages — which resolve their token+org lazily at
	// network-connect time via getDeviceConfigFromFile() rather than freezing a
	// startup snapshot — connect() re-resolves these fields through provider just
	// before dialing whenever the credentials are still incomplete. Guarded by
	// configMu because connect() runs on a background goroutine.
	configMu   sync.Mutex
	apiBaseURL string
	apiToken   string
	orgID      string
	nodeID     string
	nodeName   string

	// provider, when non-nil, returns the freshest chat credentials resolved
	// from the node's on-disk config/manifest/network state. It is consulted by
	// connect() when the seeded snapshot is missing any required field so that a
	// node authed after startup no longer shows a permanent "device
	// authorization required" status.
	provider func() ChatPageConfig

	// UI components
	root       *tview.Flex
	sidebar    *tview.Flex
	channelBox *tview.TextView
	peersBox   *tview.TextView
	msgView    *tview.TextView
	statusBar  *tview.TextView
	input      *tview.InputField
	sendButton *tview.Button

	// Chat client
	client   *chat.Client
	clientMu sync.Mutex

	// Message buffer
	messages   []chat.Message
	messagesMu sync.Mutex

	// Track recently sent message timestamps to suppress echo duplicates
	recentSends   map[string]time.Time
	recentSendsMu sync.Mutex

	// Presence tracking
	peers   map[string]chat.PresenceInfo
	peersMu sync.RWMutex

	// Lifecycle
	connected bool
	state     connState
	cancel    context.CancelFunc
}

// ChatPageConfig holds the configuration needed to create a ChatPage.
type ChatPageConfig struct {
	APIBaseURL string
	APIToken   string
	OrgID      string
	NodeID     string
	NodeName   string

	// Provider, when set, is a lazy re-resolver for the credentials above. The
	// control center constructs the ChatPage at startup — before the in-TUI
	// device-auth flow can write credentials to disk — so the snapshot embedded
	// in the fields above may be empty on a node that authorizes after launch.
	// connect() calls Provider to pick up the freshly-written credentials,
	// mirroring how the terminal/desktop/worker pages resolve their token+org
	// at network-connect time. Optional: when nil, only the snapshot is used.
	Provider func() ChatPageConfig
}

// NewChatPage creates a new chat page. The page connects lazily on first activation.
func NewChatPage(cfg ChatPageConfig) *ChatPage {
	return &ChatPage{
		apiBaseURL:  cfg.APIBaseURL,
		apiToken:    cfg.APIToken,
		orgID:       cfg.OrgID,
		nodeID:      cfg.NodeID,
		nodeName:    cfg.NodeName,
		provider:    cfg.Provider,
		messages:    make([]chat.Message, 0, 200),
		peers:       make(map[string]chat.PresenceInfo),
		recentSends: make(map[string]time.Time),
	}
}

// resolveConfig returns the current chat credentials, lazily re-resolving them
// through the provider when the seeded snapshot is missing any required field.
// A node that completes device authorization after the control center starts
// only persists its token/org at that point; without this re-resolution the
// ChatPage would keep the empty startup snapshot forever and show "device
// authorization required" even though the node is fully authed.
//
// It is safe to call from the connect() goroutine: the read/refresh of the
// config fields is serialized by configMu.
func (p *ChatPage) resolveConfig() (apiBaseURL, apiToken, orgID, nodeID, nodeName string) {
	p.configMu.Lock()
	defer p.configMu.Unlock()

	incomplete := p.apiBaseURL == "" || p.apiToken == "" || p.orgID == ""
	if incomplete && p.provider != nil {
		fresh := p.provider()
		// Only overwrite a field when the provider has a non-empty value, so a
		// transient resolution miss never clobbers a credential we already had.
		if fresh.APIBaseURL != "" {
			p.apiBaseURL = fresh.APIBaseURL
		}
		if fresh.APIToken != "" {
			p.apiToken = fresh.APIToken
		}
		if fresh.OrgID != "" {
			p.orgID = fresh.OrgID
		}
		if fresh.NodeID != "" {
			p.nodeID = fresh.NodeID
		}
		if fresh.NodeName != "" {
			p.nodeName = fresh.NodeName
		}
	}

	return p.apiBaseURL, p.apiToken, p.orgID, p.nodeID, p.nodeName
}

// currentAPIBaseURL returns the currently-known API base URL under configMu,
// for the status-bar / settings readouts. It does not trigger a provider
// re-resolution — that happens only at connect() time.
func (p *ChatPage) currentAPIBaseURL() string {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	return p.apiBaseURL
}

// Name implements Page.
func (p *ChatPage) Name() string { return "chat" }

// Title implements Page.
func (p *ChatPage) Title() string { return "Chat" }

// Build implements Page. Constructs the split-pane chat layout.
func (p *ChatPage) Build(app *tview.Application) tview.Primitive {
	p.app = app

	// -- Left sidebar: channels + online nodes --

	p.channelBox = tview.NewTextView()
	p.channelBox.SetDynamicColors(true)
	p.channelBox.SetBorder(true)
	p.channelBox.SetTitle(" Channels ")
	p.channelBox.SetTitleAlign(tview.AlignLeft)
	p.channelBox.SetText(" [green]# general[white]")

	p.peersBox = tview.NewTextView()
	p.peersBox.SetDynamicColors(true)
	p.peersBox.SetBorder(true)
	p.peersBox.SetTitle(" Online ")
	p.peersBox.SetTitleAlign(tview.AlignLeft)
	p.peersBox.SetText(" [gray]connecting...[white]")

	p.sidebar = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.channelBox, 5, 0, false).
		AddItem(p.peersBox, 0, 1, false)

	// -- Right panel: message view + input --

	p.msgView = tview.NewTextView()
	p.msgView.SetDynamicColors(true)
	p.msgView.SetScrollable(true)
	p.msgView.SetBorder(true)
	p.msgView.SetTitle(" #general ")
	p.msgView.SetTitleAlign(tview.AlignLeft)
	p.msgView.SetText(" [gray]Connecting to chat...[white]\n")

	// Status bar: shows which WSS endpoint the chat is connected to + health.
	p.statusBar = tview.NewTextView()
	p.statusBar.SetDynamicColors(true)
	p.statusBar.SetTextAlign(tview.AlignLeft)

	p.input = tview.NewInputField()
	p.input.SetLabel("> ")
	p.input.SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
	p.input.SetLabelColor(tcell.ColorGreen)
	p.input.SetPlaceholder("Type a message and press Enter (or click Send)")
	p.input.SetPlaceholderTextColor(tcell.ColorDimGray)
	p.input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			p.sendMessage()
		}
	})

	// Visible, clickable Send affordance so mouse users aren't forced to know
	// that Enter submits. Button.SetSelectedFunc fires on both keyboard Enter
	// (when focused) and MouseLeftClick, so it works either way. After sending,
	// focus returns to the input so the user can keep typing.
	p.sendButton = tview.NewButton("Send")
	p.sendButton.SetSelectedFunc(func() {
		p.sendMessage()
		if p.app != nil && p.input != nil {
			p.app.SetFocus(p.input)
		}
	})

	// Input row: the text field expands, the Send button is a fixed-width click
	// target on the right.
	inputRow := tview.NewFlex().
		AddItem(p.input, 0, 1, true).
		AddItem(p.sendButton, 8, 0, false)

	rightPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.msgView, 0, 1, false).
		AddItem(p.statusBar, 1, 0, false).
		AddItem(inputRow, 1, 0, true)

	// Render the initial "connecting" status line.
	p.renderStatusBar(connConnecting, "")

	// -- Root layout --

	p.root = tview.NewFlex().
		AddItem(p.sidebar, 24, 0, false).
		AddItem(rightPanel, 0, 1, true)

	// Mouse: clicking the message history, a channel, or a peer focuses the input
	// so a mouse user can immediately start typing (rather than the clicked pane
	// stealing focus). The scroll wheel still scrolls the message history because
	// scroll actions are passed through unchanged; only left-clicks redirect
	// focus. Keyboard navigation is unaffected — this is a mouse-only hook.
	focusInputOnClick := func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseLeftClick, tview.MouseLeftDown:
			if p.app != nil && p.input != nil {
				p.app.SetFocus(p.input)
			}
			// Consume the click so the clicked TextView does not immediately grab
			// focus back on MouseLeftDown. Returning a nil event with the
			// MouseConsumed action skips the primitive's default handler.
			return tview.MouseConsumed, nil
		default:
			// Pass scroll (and everything else) through so wheel-scrolling the
			// message history keeps working.
			return action, event
		}
	}
	p.msgView.SetMouseCapture(focusInputOnClick)
	p.channelBox.SetMouseCapture(focusInputOnClick)
	p.peersBox.SetMouseCapture(focusInputOnClick)

	return p.root
}

// OnActivate implements Page. Lazily connects the chat client on first activation.
func (p *ChatPage) OnActivate() {
	// Focus the input field
	if p.app != nil && p.input != nil {
		p.app.SetFocus(p.input)
	}

	p.clientMu.Lock()
	alreadyConnected := p.connected
	p.clientMu.Unlock()

	if alreadyConnected {
		return
	}

	// Connect in background
	go p.connect()
}

// OnDeactivate implements Page.
func (p *ChatPage) OnDeactivate() {}

// chatKeyAction classifies how the Chat page should treat a key event. It is the
// escape hatch for the keyboard trap: when the input field is focused, tview's
// InputField would otherwise *consume* Tab/Shift+Tab/Escape internally (its
// SetDoneFunc path swallows them), leaving the user unable to leave the Chat tab.
//
// The Control Center routes every key through the active page's HandleInput
// *before* it reaches the focused InputField (see PageManager.HandleGlobalInput),
// so HandleInput returning nil consumes the key before the InputField ever sees
// it — that is how we reclaim the navigation keys the InputField would trap.
type chatKeyAction int

const (
	// chatKeyPassthrough returns the event unhandled so the focused primitive
	// (usually the InputField: printable text, Enter=send, autocomplete) handles it.
	chatKeyPassthrough chatKeyAction = iota
	// chatKeyScrollUp / chatKeyScrollDown scroll the message history.
	chatKeyScrollUp
	chatKeyScrollDown
	// chatKeyDefocus moves focus off the input back to the message view (Escape),
	// so the user is no longer trapped typing.
	chatKeyDefocus
	// chatKeyNextPane / chatKeyPrevPane cycle focus among the Chat page's own
	// panes (input -> messages -> peers -> channels -> input).
	chatKeyNextPane
	chatKeyPrevPane
)

// classifyChatKey maps a key event to a chatKeyAction. Pure and table-testable
// without a running tview app. The rule (per the keyboard-trap fix): the chat
// input only consumes printable text + Enter (send) + history/scroll keys;
// everything navigational is reclaimed here so the user can always leave.
func classifyChatKey(event *tcell.EventKey) chatKeyAction {
	// Alt+<rune> is a global tab accelerator handled upstream in
	// HandleGlobalInput before it ever reaches here; never treat it as text.
	if event.Modifiers()&tcell.ModAlt != 0 {
		return chatKeyPassthrough
	}
	switch event.Key() {
	case tcell.KeyPgUp:
		return chatKeyScrollUp
	case tcell.KeyPgDn:
		return chatKeyScrollDown
	case tcell.KeyEscape:
		return chatKeyDefocus
	case tcell.KeyTab:
		return chatKeyNextPane
	case tcell.KeyBacktab:
		return chatKeyPrevPane
	default:
		return chatKeyPassthrough
	}
}

// HandleInput implements Page. It runs before the focused InputField sees the
// event, so it is where we free the navigation keys the InputField would trap.
func (p *ChatPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	switch classifyChatKey(event) {
	case chatKeyScrollUp:
		row, col := p.msgView.GetScrollOffset()
		p.msgView.ScrollTo(row-10, col)
		return nil
	case chatKeyScrollDown:
		row, col := p.msgView.GetScrollOffset()
		p.msgView.ScrollTo(row+10, col)
		return nil
	case chatKeyDefocus:
		// Escape releases the keyboard trap: move focus off the input to the
		// message view so the user can navigate (Tab/Alt+N) freely.
		if p.app != nil && p.msgView != nil {
			p.app.SetFocus(p.msgView)
		}
		return nil
	case chatKeyNextPane:
		// Cycle forward among chat panes; at the LAST pane focus doesn't move,
		// so return the (unconsumed) event and let HandleGlobalInput switch to
		// the next tab — that is how the user leaves the Chat tab by keyboard.
		if p.focusNextPane() {
			return nil
		}
		return event
	case chatKeyPrevPane:
		// Cycle backward; at the FIRST pane bubble the event so HandleGlobalInput
		// falls back to the previous tab.
		if p.focusPrevPane() {
			return nil
		}
		return event
	default:
		return event
	}
}

// chatPanes returns the Chat page's focusable panes in tab order. Kept as a
// helper so pane cycling and its tests share a single source of truth.
func (p *ChatPage) chatPanes() []tview.Primitive {
	return []tview.Primitive{p.input, p.msgView, p.peersBox, p.channelBox}
}

// nextChatPaneIndex computes the index of the pane to focus next, cycling
// forward (delta=+1) or backward (delta=-1) from the currently focused pane.
//
// Unlike a wrapping cycle, this bubbles at the pane edge so the user can leave
// the Chat tab with the same Tab/Shift+Tab that navigates within it (the shared
// navigation convention in HandleGlobalInput): moving forward past the LAST pane
// returns an out-of-range index (== count) and moving backward before the FIRST
// pane returns -1. The caller treats any out-of-range result as "past the edge"
// and returns the key event UNCONSUMED so HandleGlobalInput switches tabs. This
// is what un-traps the Chat tab — the previous wrapping cycle (input->messages->
// peers->channels->input forever) always consumed Tab and never bubbled.
//
// Pure and testable: `focused` is the index of the currently focused pane, or
// -1 when focus is elsewhere. When focus is elsewhere we do NOT bubble — we
// enter the pane list at the input (forward) or the last pane (backward), so a
// Tab from the off-list sendButton lands on a pane rather than leaving the tab.
func nextChatPaneIndex(focused, count, delta int) int {
	if count == 0 {
		return -1
	}
	if focused < 0 {
		if delta >= 0 {
			return 0
		}
		return count - 1
	}
	// Deliberately no modulo wrap: forward-from-last yields count and
	// backward-from-first yields -1, both out of range, signalling "bubble".
	return focused + delta
}

// currentChatPaneIndex returns the index of the currently focused chat pane, or
// -1 if focus is not on any of them.
func (p *ChatPage) currentChatPaneIndex() int {
	if p.app == nil {
		return -1
	}
	focused := p.app.GetFocus()
	for i, pane := range p.chatPanes() {
		if pane == focused {
			return i
		}
	}
	return -1
}

// cycleChatPane moves focus to the next/previous chat pane. It reports whether
// focus actually moved: false means the target index is past the pane edge (or
// there are no panes), i.e. the caller should bubble the key up for tab
// switching rather than consuming it. Guarding BOTH ends is load-bearing — the
// forward bubble sentinel is `len(panes)` (a positive, in-range-looking value),
// so a lone `idx < 0` check would index panes[len(panes)] and panic.
func (p *ChatPage) cycleChatPane(delta int) bool {
	panes := p.chatPanes()
	idx := nextChatPaneIndex(p.currentChatPaneIndex(), len(panes), delta)
	if idx < 0 || idx >= len(panes) {
		return false
	}
	if p.app != nil {
		p.app.SetFocus(panes[idx])
	}
	return true
}

func (p *ChatPage) focusNextPane() bool { return p.cycleChatPane(1) }
func (p *ChatPage) focusPrevPane() bool { return p.cycleChatPane(-1) }

// connect establishes the chat client connection.
func (p *ChatPage) connect() {
	p.clientMu.Lock()
	if p.connected {
		p.clientMu.Unlock()
		return
	}

	// Re-resolve credentials before dialing. On a node that device-authed after
	// the control center launched, the snapshot captured at construction is
	// empty; resolveConfig picks up the credentials device-auth wrote to disk,
	// matching the lazy resolution the terminal/desktop/worker pages already do.
	apiBaseURL, apiToken, orgID, nodeID, nodeName := p.resolveConfig()

	if apiBaseURL == "" || apiToken == "" || orgID == "" {
		p.clientMu.Unlock()
		// Surface the failure in BOTH the message view AND the status bar. The
		// status bar was seeded to connConnecting in Build(); without this it
		// stays pinned on the "connecting…" spinner forever even though we never
		// dial. Flip it to a real error state instead.
		p.setStatus("[red]Not configured[white] — device authorization required")
		p.app.QueueUpdateDraw(func() {
			p.renderStatusBar(connError, "device authorization required")
		})
		return
	}

	client := chat.NewClient(chat.ClientConfig{
		APIBaseURL: apiBaseURL,
		Token:      apiToken,
		OrgID:      orgID,
		NodeID:     nodeID,
		NodeName:   nodeName,
	})
	p.client = client
	p.clientMu.Unlock()

	// Show the (sanitized) endpoint we are dialing while connecting.
	p.app.QueueUpdateDraw(func() {
		p.renderStatusBar(connConnecting, "")
	})

	// Wire up callbacks
	client.OnMessage(func(msg chat.Message) {
		// Suppress echo of own messages (already rendered via local echo)
		if msg.FromNodeID == p.nodeID {
			key := msg.Body + "|" + msg.Timestamp.Format(time.RFC3339)
			p.recentSendsMu.Lock()
			if _, dup := p.recentSends[key]; dup {
				delete(p.recentSends, key)
				p.recentSendsMu.Unlock()
				return
			}
			p.recentSendsMu.Unlock()
		}

		p.messagesMu.Lock()
		p.messages = append(p.messages, msg)
		// Keep last 200 messages
		if len(p.messages) > 200 {
			p.messages = p.messages[len(p.messages)-200:]
		}
		p.messagesMu.Unlock()

		p.app.QueueUpdateDraw(func() {
			p.appendMessage(msg)
		})
	})

	client.OnPresence(func(info chat.PresenceInfo) {
		p.peersMu.Lock()
		p.peers[info.NodeID] = info
		p.peersMu.Unlock()

		p.app.QueueUpdateDraw(func() {
			p.updatePeersView()
		})
	})

	// OnConnect fires only after the real-time transport handshake + subscribe
	// succeed. Until then the UI stays in the "connecting" state, so a failed
	// handshake no longer renders as a false "Connected".
	client.OnConnect(func() {
		p.clientMu.Lock()
		p.connected = true
		p.clientMu.Unlock()

		p.app.QueueUpdateDraw(func() {
			p.renderStatusBar(connConnected, "")
			// Already on the main goroutine inside QueueUpdateDraw: use the
			// direct writeStatus, NOT setStatus (which re-enters QueueUpdateDraw
			// and deadlocks the event loop).
			p.writeStatus("[green]Connected[white] to #general")
			// Clear the "connecting..." placeholder in the peers sidebar.
			p.updatePeersView()
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	// Watchdog: if OnConnect has not fired within connectTimeout of dialing, flip
	// the status bar out of the "connecting…" spinner into a real timeout error.
	// This guarantees the UI never spins forever, regardless of the failure mode
	// (a hung dial, a server that closes on a scope mismatch, or a silently
	// rejected subscribe). It races the successful-connect path, so it must
	// re-check p.connected / ctx.Err() under the lock — mirroring the guard on the
	// Connect() error branch below — and no-op if either indicates success or
	// shutdown.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(connectTimeout):
		}
		p.clientMu.Lock()
		connected := p.connected
		p.clientMu.Unlock()
		if !connected && ctx.Err() == nil {
			p.app.QueueUpdateDraw(func() {
				p.renderStatusBar(connError, "timed out")
				// Inside QueueUpdateDraw: use writeStatus, not setStatus.
				p.writeStatus("[red]Connection timed out[white] — chat is unavailable")
				p.peersBox.SetText(" [red]offline[white]")
			})
		}
	}()

	// Connect blocks until ctx is cancelled on success, or returns quickly with
	// an error if the handshake/subscribe fails. Surface the real error instead
	// of leaving the UI stuck on "connecting...".
	if err := client.Connect(ctx); err != nil {
		p.clientMu.Lock()
		wasConnected := p.connected
		p.connected = false
		p.clientMu.Unlock()

		// A nil-cancel context error (context.Canceled) on a clean shutdown is
		// not a real failure — only report errors that occurred before/without
		// a successful connect.
		if !wasConnected && ctx.Err() == nil {
			errMsg := err.Error()
			p.app.QueueUpdateDraw(func() {
				p.renderStatusBar(connError, errMsg)
				// Inside QueueUpdateDraw: use writeStatus, not setStatus.
				p.writeStatus(fmt.Sprintf("[red]Connection failed[white]: %s", errMsg))
				p.peersBox.SetText(" [red]offline[white]")
			})
		}
	}
}

// sendMessage sends the current input as a chat message.
func (p *ChatPage) sendMessage() {
	text := strings.TrimSpace(p.input.GetText())
	if text == "" {
		return
	}

	p.clientMu.Lock()
	client := p.client
	p.clientMu.Unlock()

	if client == nil {
		return
	}

	p.input.SetText("")

	// Local echo: render immediately so the sender sees their message
	now := time.Now().UTC()
	localMsg := chat.Message{
		FromNodeID:   p.nodeID,
		FromNodeName: p.nodeName,
		Channel:      "general",
		Body:         text,
		Timestamp:    now,
	}

	// Track for dedup when the server echo arrives
	key := text + "|" + now.Format(time.RFC3339)
	p.recentSendsMu.Lock()
	p.recentSends[key] = now
	p.recentSendsMu.Unlock()

	p.messagesMu.Lock()
	p.messages = append(p.messages, localMsg)
	if len(p.messages) > 200 {
		p.messages = p.messages[len(p.messages)-200:]
	}
	p.messagesMu.Unlock()

	p.appendMessage(localMsg)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := client.Send(ctx, text); err != nil {
			p.app.QueueUpdateDraw(func() {
				fmt.Fprintf(p.msgView, " [red]Send failed: %s[white]\n", err.Error())
				p.msgView.ScrollToEnd()
			})

			// Remove dedup entry on failure so re-sends aren't suppressed
			p.recentSendsMu.Lock()
			delete(p.recentSends, key)
			p.recentSendsMu.Unlock()
		}
	}()
}

// appendMessage renders a single message in the message view.
func (p *ChatPage) appendMessage(msg chat.Message) {
	ts := msg.Timestamp.Local().Format("15:04")
	name := msg.FromNodeName
	if name == "" {
		name = msg.FromNodeID
	}

	// Highlight own messages
	nameColor := "cyan"
	if msg.FromNodeID == p.nodeID {
		nameColor = "green"
	}

	fmt.Fprintf(p.msgView, " [gray]%s[white] [%s]%s[white]: %s\n",
		ts, nameColor, tview.Escape(name), tview.Escape(msg.Body))
	p.msgView.ScrollToEnd()
}

// formatChatStatusBar renders the one-line status text for a given connection
// state, endpoint, and optional error detail. Pure (no tview app required) so
// the connecting/connected/error transitions are unit-testable. The endpoint is
// assumed already sanitized to scheme + host by the caller.
func formatChatStatusBar(state connState, endpoint, detail string) string {
	if endpoint == "" {
		endpoint = "not configured"
	}
	switch state {
	case connConnected:
		return fmt.Sprintf(" [green]●[white] connected  [gray]%s[white]", tview.Escape(endpoint))
	case connError:
		msg := fmt.Sprintf(" [red]●[white] error  [gray]%s[white]", tview.Escape(endpoint))
		if detail != "" {
			msg += fmt.Sprintf("  [red]%s[white]", tview.Escape(detail))
		}
		return msg
	default:
		return fmt.Sprintf(" [yellow]●[white] connecting…  [gray]%s[white]", tview.Escape(endpoint))
	}
}

// renderStatusBar updates the one-line connection status indicator beneath the
// message view. It surfaces which WSS endpoint the chat is using and its health
// (connecting / connected / error). The endpoint is sanitized to scheme + host
// so the underlying transport path is never exposed to the user.
func (p *ChatPage) renderStatusBar(state connState, detail string) {
	if p.statusBar == nil {
		return
	}

	p.state = state
	endpoint := chat.SanitizeEndpoint(p.currentAPIBaseURL())
	p.statusBar.SetText(formatChatStatusBar(state, endpoint, detail))
}

// writeStatus replaces the message view with a single status line. It touches
// the tview primitive DIRECTLY (no QueueUpdateDraw) and therefore MUST only be
// called from the main/event-loop goroutine — i.e. from inside an existing
// QueueUpdateDraw callback. Background-goroutine callers must use setStatus.
//
// This split exists to prevent a nested-QueueUpdateDraw deadlock: setStatus
// wraps the body in QueueUpdateDraw, so calling it from within another
// QueueUpdateDraw callback (already on the main goroutine) re-enters
// QueueUpdateDraw, whose synchronous channel receive waits for the very event
// loop it is blocking — a permanent hang that freezes the entire Control Center.
func (p *ChatPage) writeStatus(text string) {
	if p.msgView == nil {
		return
	}
	p.msgView.Clear()
	fmt.Fprintf(p.msgView, " %s\n\n", text)
	p.msgView.ScrollToEnd()
}

// setStatus updates the message view with a status line from a background
// goroutine. It wraps writeStatus in QueueUpdateDraw to hop onto the main
// goroutine, so it MUST NOT be called from within an existing QueueUpdateDraw
// callback — use writeStatus directly in that case (see writeStatus).
func (p *ChatPage) setStatus(text string) {
	p.app.QueueUpdateDraw(func() {
		p.writeStatus(text)
	})
}

// updatePeersView refreshes the online peers sidebar.
func (p *ChatPage) updatePeersView() {
	p.peersMu.RLock()
	defer p.peersMu.RUnlock()

	if len(p.peers) == 0 {
		p.peersBox.SetText(" [gray]no peers yet[white]")
		return
	}

	// Sort peers by name
	sorted := make([]chat.PresenceInfo, 0, len(p.peers))
	for _, peer := range p.peers {
		sorted = append(sorted, peer)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].NodeName < sorted[j].NodeName
	})

	var sb strings.Builder
	for _, peer := range sorted {
		name := peer.NodeName
		if name == "" {
			name = peer.NodeID
		}
		if peer.IsOnline(presenceTimeout) {
			fmt.Fprintf(&sb, " [green]●[white] %s\n", tview.Escape(name))
		} else {
			fmt.Fprintf(&sb, " [gray]○[white] [gray]%s[white]\n", tview.Escape(name))
		}
	}

	p.peersBox.SetText(sb.String())
}

// ConnState describes the high-level connection state of the chat/control link.
type ConnState string

const (
	ConnDisconnected ConnState = "disconnected"
	ConnConnecting   ConnState = "connecting"
	ConnConnected    ConnState = "connected"
)

// ConnectionStatus reports the user-facing connection status of the realtime
// link: the WSS endpoint host and whether it is connected. The underlying
// transport detail (Redis pub/sub) is intentionally not surfaced — callers
// (e.g. the Settings page) should present this as a generic "connection".
func (p *ChatPage) ConnectionStatus() (endpoint string, state ConnState) {
	endpoint = wssEndpoint(p.currentAPIBaseURL())

	p.clientMu.Lock()
	client := p.client
	connecting := p.connected
	p.clientMu.Unlock()

	switch {
	case client != nil && client.IsConnected():
		return endpoint, ConnConnected
	case connecting:
		return endpoint, ConnConnecting
	default:
		return endpoint, ConnDisconnected
	}
}

// Close shuts down the chat page and its client.
func (p *ChatPage) Close() {
	if p.cancel != nil {
		p.cancel()
	}
	p.clientMu.Lock()
	if p.client != nil {
		_ = p.client.Close()
		p.client = nil
	}
	p.connected = false
	p.clientMu.Unlock()
}
