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

	// Config (set at construction, read-only after)
	apiBaseURL string
	apiToken   string
	orgID      string
	nodeID     string
	nodeName   string

	// UI components
	root       *tview.Flex
	sidebar    *tview.Flex
	channelBox *tview.TextView
	peersBox   *tview.TextView
	msgView    *tview.TextView
	statusBar  *tview.TextView
	input      *tview.InputField

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
}

// NewChatPage creates a new chat page. The page connects lazily on first activation.
func NewChatPage(cfg ChatPageConfig) *ChatPage {
	return &ChatPage{
		apiBaseURL:  cfg.APIBaseURL,
		apiToken:    cfg.APIToken,
		orgID:       cfg.OrgID,
		nodeID:      cfg.NodeID,
		nodeName:    cfg.NodeName,
		messages:    make([]chat.Message, 0, 200),
		peers:       make(map[string]chat.PresenceInfo),
		recentSends: make(map[string]time.Time),
	}
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
	p.input.SetPlaceholder("Type a message and press Enter")
	p.input.SetPlaceholderTextColor(tcell.ColorDimGray)
	p.input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			p.sendMessage()
		}
	})

	rightPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.msgView, 0, 1, false).
		AddItem(p.statusBar, 1, 0, false).
		AddItem(p.input, 1, 0, true)

	// Render the initial "connecting" status line.
	p.renderStatusBar(connConnecting, "")

	// -- Root layout --

	p.root = tview.NewFlex().
		AddItem(p.sidebar, 24, 0, false).
		AddItem(rightPanel, 0, 1, true)

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

// HandleInput implements Page. Routes input to the input field or message view.
func (p *ChatPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	// Page Up / Page Down scroll the message view
	switch event.Key() {
	case tcell.KeyPgUp:
		row, col := p.msgView.GetScrollOffset()
		p.msgView.ScrollTo(row-10, col)
		return nil
	case tcell.KeyPgDn:
		row, col := p.msgView.GetScrollOffset()
		p.msgView.ScrollTo(row+10, col)
		return nil
	}

	return event
}

// connect establishes the chat client connection.
func (p *ChatPage) connect() {
	p.clientMu.Lock()
	if p.connected {
		p.clientMu.Unlock()
		return
	}

	if p.apiBaseURL == "" || p.apiToken == "" || p.orgID == "" {
		p.clientMu.Unlock()
		p.setStatus("[red]Not configured[white] — device authorization required")
		return
	}

	client := chat.NewClient(chat.ClientConfig{
		APIBaseURL: p.apiBaseURL,
		Token:      p.apiToken,
		OrgID:      p.orgID,
		NodeID:     p.nodeID,
		NodeName:   p.nodeName,
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
			p.setStatus("[green]Connected[white] to #general")
			// Clear the "connecting..." placeholder in the peers sidebar.
			p.updatePeersView()
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

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
				p.setStatus(fmt.Sprintf("[red]Connection failed[white]: %s", errMsg))
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

// renderStatusBar updates the one-line connection status indicator beneath the
// message view. It surfaces which WSS endpoint the chat is using and its health
// (connecting / connected / error). The endpoint is sanitized to scheme + host
// so the underlying transport path is never exposed to the user.
func (p *ChatPage) renderStatusBar(state connState, detail string) {
	if p.statusBar == nil {
		return
	}

	p.state = state
	endpoint := chat.SanitizeEndpoint(p.apiBaseURL)
	if endpoint == "" {
		endpoint = "not configured"
	}

	switch state {
	case connConnected:
		p.statusBar.SetText(fmt.Sprintf(" [green]●[white] connected  [gray]%s[white]", tview.Escape(endpoint)))
	case connError:
		msg := fmt.Sprintf(" [red]●[white] error  [gray]%s[white]", tview.Escape(endpoint))
		if detail != "" {
			msg += fmt.Sprintf("  [red]%s[white]", tview.Escape(detail))
		}
		p.statusBar.SetText(msg)
	default:
		p.statusBar.SetText(fmt.Sprintf(" [yellow]●[white] connecting…  [gray]%s[white]", tview.Escape(endpoint)))
	}
}

// setStatus updates the message view with a status line.
func (p *ChatPage) setStatus(text string) {
	p.app.QueueUpdateDraw(func() {
		p.msgView.Clear()
		fmt.Fprintf(p.msgView, " %s\n\n", text)
		p.msgView.ScrollToEnd()
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
	endpoint = wssEndpoint(p.apiBaseURL)

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
