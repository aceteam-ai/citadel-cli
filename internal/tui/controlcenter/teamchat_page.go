package controlcenter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/teamchat"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// teamChatPollInterval is how often the active channel is re-fetched while the
// Team Chat tab is open. Matches the iOS/Android light-polling cadence — the
// platform's v1 chat WebSocket is gone pending the v2 cutover, so polling is
// the cross-client parity mechanism (issue #495).
const teamChatPollInterval = 5 * time.Second

// teamChatPageSize is the message page size fetched per poll/load.
const teamChatPageSize = 50

// TeamChatPage implements the Page interface for the Team Chat tab. It mirrors
// the AceTeam Team Chat product surface (web /team-chat, iOS, Android): org
// channels backed by the channels/channel_messages tables, read and written
// through the v1 REST API via the teamchat client.
//
// This is distinct from the ChatPage ("Node Chat"), which is an ephemeral
// node-to-node pub/sub chat over the Redis proxy. Team Chat requires a user
// act_ API key until device tokens are allowed onto /api/channels/** (#495).
type TeamChatPage struct {
	app *tview.Application

	// Config, resolved lazily like ChatPage: a node that completes device
	// authorization (or exports ACETEAM_API_KEY) after startup should pick up
	// credentials at connect time rather than freezing the startup snapshot.
	configMu sync.Mutex
	cfg      TeamChatPageConfig

	// UI components
	root        *tview.Flex
	channelList *tview.List
	membersBox  *tview.TextView
	msgView     *tview.TextView
	statusBar   *tview.TextView
	input       *tview.InputField
	sendButton  *tview.Button

	// Client + data. dataMu guards everything below.
	dataMu        sync.Mutex
	client        *teamchat.Client
	channels      []teamchat.Channel
	activeChannel string // channel ID; empty until channels load
	messages      []teamchat.Message
	lastMessageID string // newest rendered message; drives re-render + mark-read
	loadedOnce    bool   // first successful channel-list load happened

	// Poll lifecycle. pollCancel is non-nil while the page is active.
	pollMu     sync.Mutex
	pollCancel context.CancelFunc
}

// TeamChatPageConfig holds the configuration for the Team Chat page.
type TeamChatPageConfig struct {
	// APIBaseURL is the AceTeam API base URL (e.g. "https://aceteam.ai").
	APIBaseURL string
	// Token is the Bearer credential (user act_ key preferred; the device
	// token is scope-denied on Team Chat routes today, see #495).
	Token string
	// TokenSource labels where Token came from ("env", "config", "device"),
	// so the UI can warn when only the device token is available.
	TokenSource string
	// Provider, when set, lazily re-resolves the fields above at connect time
	// (same pattern as ChatPageConfig.Provider).
	Provider func() TeamChatPageConfig
}

// NewTeamChatPage creates the Team Chat page. Data loads lazily on first
// activation.
func NewTeamChatPage(cfg TeamChatPageConfig) *TeamChatPage {
	return &TeamChatPage{cfg: cfg}
}

// Name implements Page.
func (p *TeamChatPage) Name() string { return "teamchat" }

// Title implements Page.
func (p *TeamChatPage) Title() string { return "Team Chat" }

// Build implements Page.
func (p *TeamChatPage) Build(app *tview.Application) tview.Primitive {
	p.app = app

	// -- Left sidebar: channel list + members --

	p.channelList = tview.NewList()
	p.channelList.ShowSecondaryText(false)
	p.channelList.SetBorder(true)
	p.channelList.SetTitle(" Channels ")
	p.channelList.SetTitleAlign(tview.AlignLeft)
	p.channelList.SetSelectedFunc(func(index int, mainText, secondaryText string, shortcut rune) {
		p.onChannelSelected(index)
	})

	p.membersBox = tview.NewTextView()
	p.membersBox.SetDynamicColors(true)
	p.membersBox.SetBorder(true)
	p.membersBox.SetTitle(" Members ")
	p.membersBox.SetTitleAlign(tview.AlignLeft)

	sidebar := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.channelList, 0, 2, false).
		AddItem(p.membersBox, 0, 1, false)

	// -- Right panel: messages + status + input --

	p.msgView = tview.NewTextView()
	p.msgView.SetDynamicColors(true)
	p.msgView.SetScrollable(true)
	p.msgView.SetBorder(true)
	p.msgView.SetTitle(" Team Chat ")
	p.msgView.SetTitleAlign(tview.AlignLeft)
	p.msgView.SetText(" [gray]Loading Team Chat...[white]\n")

	p.statusBar = tview.NewTextView()
	p.statusBar.SetDynamicColors(true)
	p.statusBar.SetTextAlign(tview.AlignLeft)

	p.input = tview.NewInputField()
	p.input.SetLabel("> ")
	p.input.SetFieldBackgroundColor(tcell.ColorDarkSlateGray)
	p.input.SetLabelColor(tcell.ColorGreen)
	p.input.SetPlaceholder("Message — Enter to send, /search <text> to search")
	p.input.SetPlaceholderTextColor(tcell.ColorDimGray)
	p.input.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			p.submitInput()
		}
	})

	p.sendButton = tview.NewButton("Send")
	p.sendButton.SetSelectedFunc(func() {
		p.submitInput()
		if p.app != nil && p.input != nil {
			p.app.SetFocus(p.input)
		}
	})

	inputRow := tview.NewFlex().
		AddItem(p.input, 0, 1, true).
		AddItem(p.sendButton, 8, 0, false)

	rightPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(p.msgView, 0, 1, false).
		AddItem(p.statusBar, 1, 0, false).
		AddItem(inputRow, 1, 0, true)

	p.renderStatus(connConnecting, "")

	p.root = tview.NewFlex().
		AddItem(sidebar, 28, 0, false).
		AddItem(rightPanel, 0, 1, true)

	// Clicking the message history or members focuses the input (same
	// mouse-only hook as ChatPage); the channel list keeps real clicks so
	// mouse users can switch channels.
	focusInputOnClick := func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseLeftClick, tview.MouseLeftDown:
			if p.app != nil && p.input != nil {
				p.app.SetFocus(p.input)
			}
			return tview.MouseConsumed, nil
		default:
			return action, event
		}
	}
	p.msgView.SetMouseCapture(focusInputOnClick)
	p.membersBox.SetMouseCapture(focusInputOnClick)

	return p.root
}

// OnActivate implements Page. Starts (or restarts) the poll loop and performs
// the initial channel load.
func (p *TeamChatPage) OnActivate() {
	if p.app != nil && p.input != nil {
		p.app.SetFocus(p.input)
	}

	p.pollMu.Lock()
	if p.pollCancel != nil {
		p.pollMu.Unlock()
		return // already polling
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.pollCancel = cancel
	p.pollMu.Unlock()

	go p.run(ctx)
}

// OnDeactivate implements Page. Stops polling — mobile parity: no background
// fetching while the tab is not visible.
func (p *TeamChatPage) OnDeactivate() {
	p.stopPolling()
}

// Close shuts down the page.
func (p *TeamChatPage) Close() {
	p.stopPolling()
}

func (p *TeamChatPage) stopPolling() {
	p.pollMu.Lock()
	if p.pollCancel != nil {
		p.pollCancel()
		p.pollCancel = nil
	}
	p.pollMu.Unlock()
}

// HandleInput implements Page. Same navigation contract as ChatPage: the
// input consumes text + Enter; PgUp/PgDn scroll; Escape defocuses; Tab cycles
// panes and bubbles at the edges so the user can always leave the tab.
func (p *TeamChatPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
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
		if p.app != nil && p.msgView != nil {
			p.app.SetFocus(p.msgView)
		}
		return nil
	case chatKeyNextPane:
		if p.cyclePane(1) {
			return nil
		}
		return event
	case chatKeyPrevPane:
		if p.cyclePane(-1) {
			return nil
		}
		return event
	default:
		return event
	}
}

// panes returns the focusable panes in tab order.
func (p *TeamChatPage) panes() []tview.Primitive {
	return []tview.Primitive{p.input, p.msgView, p.channelList, p.membersBox}
}

func (p *TeamChatPage) currentPaneIndex() int {
	if p.app == nil {
		return -1
	}
	focused := p.app.GetFocus()
	for i, pane := range p.panes() {
		if pane == focused {
			return i
		}
	}
	return -1
}

// cyclePane moves focus to the next/previous pane, reporting false when the
// cycle bubbles past an edge (caller lets the tab switch). Reuses the shared
// nextChatPaneIndex edge-bubbling helper.
func (p *TeamChatPage) cyclePane(delta int) bool {
	panes := p.panes()
	idx := nextChatPaneIndex(p.currentPaneIndex(), len(panes), delta)
	if idx < 0 || idx >= len(panes) {
		return false
	}
	if p.app != nil {
		p.app.SetFocus(panes[idx])
	}
	return true
}

// resolveClient returns the teamchat client, creating it from the freshest
// credentials when needed. Returns nil (with guidance already rendered) when
// no token is available at all.
func (p *TeamChatPage) resolveClient() *teamchat.Client {
	p.dataMu.Lock()
	if p.client != nil {
		client := p.client
		p.dataMu.Unlock()
		return client
	}
	p.dataMu.Unlock()

	p.configMu.Lock()
	if (p.cfg.Token == "" || p.cfg.APIBaseURL == "") && p.cfg.Provider != nil {
		fresh := p.cfg.Provider()
		if fresh.APIBaseURL != "" {
			p.cfg.APIBaseURL = fresh.APIBaseURL
		}
		if fresh.Token != "" {
			p.cfg.Token = fresh.Token
			p.cfg.TokenSource = fresh.TokenSource
		}
	}
	cfg := p.cfg
	p.configMu.Unlock()

	if cfg.APIBaseURL == "" || cfg.Token == "" {
		p.showAuthGuidance("no credentials configured")
		return nil
	}

	client := teamchat.NewClient(teamchat.ClientConfig{
		BaseURL: cfg.APIBaseURL,
		Token:   cfg.Token,
	})
	p.dataMu.Lock()
	p.client = client
	p.dataMu.Unlock()
	return client
}

// run is the page's background loop: initial load, then poll ticks until ctx
// is cancelled.
func (p *TeamChatPage) run(ctx context.Context) {
	client := p.resolveClient()
	if client == nil {
		return
	}

	p.loadChannels(ctx, client)

	ticker := time.NewTicker(teamChatPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollActiveChannel(ctx, client)
		}
	}
}

// loadChannels fetches the channel list, renders it, and selects the first
// channel when nothing is selected yet.
func (p *TeamChatPage) loadChannels(ctx context.Context, client *teamchat.Client) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	channels, err := client.ListChannels(reqCtx)
	cancel()
	if err != nil {
		p.handleFetchError(err)
		return
	}

	p.dataMu.Lock()
	p.channels = channels
	p.loadedOnce = true
	active := p.activeChannel
	if active == "" && len(channels) > 0 {
		active = channels[0].ID
		p.activeChannel = active
	}
	p.dataMu.Unlock()

	p.app.QueueUpdateDraw(func() {
		p.renderChannelList()
		p.renderStatus(connConnected, "")
	})

	if active != "" {
		p.loadChannel(ctx, client, active)
	} else {
		p.setMsgText(" [gray]No channels visible to your organization.[white]\n")
	}
}

// loadChannel fetches history + members for a channel and renders both.
func (p *TeamChatPage) loadChannel(ctx context.Context, client *teamchat.Client, channelID string) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	page, err := client.Messages(reqCtx, channelID, teamchat.MessagesOptions{Limit: teamChatPageSize})
	cancel()
	if err != nil {
		p.handleFetchError(err)
		return
	}

	p.dataMu.Lock()
	if p.activeChannel != channelID {
		p.dataMu.Unlock()
		return // user switched away while we were loading
	}
	p.messages = page.Messages
	newest := newestMessageID(page.Messages)
	changed := newest != p.lastMessageID
	p.lastMessageID = newest
	p.dataMu.Unlock()

	p.app.QueueUpdateDraw(func() {
		p.renderMessages()
		p.renderStatus(connConnected, "")
	})

	if changed && newest != "" {
		p.markRead(ctx, client, channelID, newest)
	}

	// Members are secondary; fetch after messages render.
	reqCtx, cancel = context.WithTimeout(ctx, 15*time.Second)
	members, err := client.ListMembers(reqCtx, channelID)
	cancel()
	if err == nil {
		p.app.QueueUpdateDraw(func() {
			p.renderMembers(members)
		})
	}
}

// pollActiveChannel re-fetches the newest page for the active channel and
// re-renders only when the newest message changed.
func (p *TeamChatPage) pollActiveChannel(ctx context.Context, client *teamchat.Client) {
	p.dataMu.Lock()
	channelID := p.activeChannel
	loaded := p.loadedOnce
	p.dataMu.Unlock()

	if !loaded {
		p.loadChannels(ctx, client)
		return
	}
	if channelID == "" {
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	page, err := client.Messages(reqCtx, channelID, teamchat.MessagesOptions{Limit: teamChatPageSize})
	cancel()
	if err != nil {
		p.handleFetchError(err)
		return
	}

	newest := newestMessageID(page.Messages)

	p.dataMu.Lock()
	if p.activeChannel != channelID {
		p.dataMu.Unlock()
		return
	}
	changed := newest != p.lastMessageID || len(page.Messages) != len(p.messages)
	p.messages = page.Messages
	p.lastMessageID = newest
	p.dataMu.Unlock()

	if changed {
		p.app.QueueUpdateDraw(func() {
			p.renderMessages()
			p.renderStatus(connConnected, "")
		})
		if newest != "" {
			p.markRead(ctx, client, channelID, newest)
		}
	}
}

// markRead advances the read cursor, best-effort.
func (p *TeamChatPage) markRead(ctx context.Context, client *teamchat.Client, channelID, messageID string) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = client.MarkRead(reqCtx, channelID, messageID)
}

// onChannelSelected handles a channel list selection (keyboard or mouse).
func (p *TeamChatPage) onChannelSelected(index int) {
	p.dataMu.Lock()
	if index < 0 || index >= len(p.channels) {
		p.dataMu.Unlock()
		return
	}
	channelID := p.channels[index].ID
	if channelID == p.activeChannel {
		p.dataMu.Unlock()
		if p.app != nil && p.input != nil {
			p.app.SetFocus(p.input)
		}
		return
	}
	p.activeChannel = channelID
	p.messages = nil
	p.lastMessageID = ""
	client := p.client
	p.dataMu.Unlock()

	p.msgView.Clear()
	fmt.Fprint(p.msgView, " [gray]Loading messages...[white]\n")
	p.renderChannelTitle()
	if p.app != nil && p.input != nil {
		p.app.SetFocus(p.input)
	}

	if client != nil {
		go p.loadChannel(context.Background(), client, channelID)
	}
}

// submitInput sends the input field's content: either a /search command or a
// chat message to the active channel.
func (p *TeamChatPage) submitInput() {
	text := strings.TrimSpace(p.input.GetText())
	if text == "" {
		return
	}

	if query, ok := parseSearchCommand(text); ok {
		p.input.SetText("")
		go p.runSearch(query)
		return
	}

	p.dataMu.Lock()
	client := p.client
	channelID := p.activeChannel
	p.dataMu.Unlock()
	if client == nil || channelID == "" {
		return
	}

	p.input.SetText("")

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		msg, err := client.SendMessage(ctx, channelID, text, "")
		if err != nil {
			errMsg := err.Error()
			p.app.QueueUpdateDraw(func() {
				fmt.Fprintf(p.msgView, " [red]Send failed: %s[white]\n", tview.Escape(errMsg))
				p.msgView.ScrollToEnd()
			})
			return
		}

		// Append the server-confirmed message immediately; the next poll
		// replaces the buffer wholesale, and ID-based state means no echo
		// dedup is needed (unlike the pub/sub Node Chat).
		p.dataMu.Lock()
		if p.activeChannel == channelID {
			p.messages = append(p.messages, msg)
			p.lastMessageID = msg.ID
		}
		p.dataMu.Unlock()

		p.app.QueueUpdateDraw(func() {
			p.appendMessage(msg)
		})
	}()
}

// runSearch executes a /search command against the active channel and renders
// the results in the message view. The next poll re-render returns to the
// live channel view.
func (p *TeamChatPage) runSearch(query string) {
	p.dataMu.Lock()
	client := p.client
	channelID := p.activeChannel
	p.dataMu.Unlock()
	if client == nil || channelID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	results, err := client.SearchMessages(ctx, channelID, query, 20)
	if err != nil {
		errMsg := err.Error()
		p.app.QueueUpdateDraw(func() {
			fmt.Fprintf(p.msgView, " [red]Search failed: %s[white]\n", tview.Escape(errMsg))
			p.msgView.ScrollToEnd()
		})
		return
	}

	p.app.QueueUpdateDraw(func() {
		p.msgView.Clear()
		fmt.Fprintf(p.msgView, " [yellow]Search results for %q[white] (%d)\n\n", query, len(results))
		if len(results) == 0 {
			fmt.Fprint(p.msgView, " [gray]No matches in this channel.[white]\n")
		}
		for _, msg := range results {
			p.writeMessageLine(msg)
		}
		fmt.Fprint(p.msgView, "\n [gray]Live view resumes on the next update.[white]\n")
		p.msgView.ScrollToEnd()
	})
}

// handleFetchError routes an API failure to the right UI: credential guidance
// for auth errors, a transient status-bar error otherwise.
func (p *TeamChatPage) handleFetchError(err error) {
	if teamchat.IsAuthError(err) {
		p.showAuthGuidance(err.Error())
		return
	}
	errMsg := err.Error()
	p.app.QueueUpdateDraw(func() {
		p.renderStatus(connError, errMsg)
	})
}

// showAuthGuidance renders actionable credential setup instructions. This is
// the expected state on a node that only has its device token: device keys
// are endpoint-whitelisted away from /api/channels/** until the #495 backend
// follow-up lands.
func (p *TeamChatPage) showAuthGuidance(detail string) {
	p.configMu.Lock()
	base := p.cfg.APIBaseURL
	source := p.cfg.TokenSource
	p.configMu.Unlock()
	if base == "" {
		base = "https://aceteam.ai"
	}

	var b strings.Builder
	b.WriteString(" [yellow]Team Chat needs an AceTeam API key.[white]\n\n")
	if source == "device" {
		b.WriteString(" This node's device token cannot access Team Chat yet\n")
		b.WriteString(" (its API scope is limited to fabric endpoints — see\n")
		b.WriteString(" citadel-cli#495 for the backend follow-up).\n\n")
	}
	b.WriteString(" To connect:\n")
	fmt.Fprintf(&b, "   1. Generate a key at [green]%s/settings/api-keys[white]\n", tview.Escape(base))
	b.WriteString("   2. Provide it via [green]ACETEAM_API_KEY[white] env var, or add\n")
	b.WriteString("      [green]aceteam_api_key: act_...[white] to your citadel config.yaml\n")
	b.WriteString("   3. Restart the control center\n")

	text := b.String()
	p.app.QueueUpdateDraw(func() {
		p.msgView.Clear()
		fmt.Fprint(p.msgView, text)
		p.renderStatus(connError, detail)
	})
}

// setMsgText replaces the message view content from a background goroutine.
func (p *TeamChatPage) setMsgText(text string) {
	p.app.QueueUpdateDraw(func() {
		p.msgView.Clear()
		fmt.Fprint(p.msgView, text)
	})
}

// renderChannelList redraws the sidebar channel list. Must run on the main
// goroutine.
func (p *TeamChatPage) renderChannelList() {
	p.dataMu.Lock()
	channels := make([]teamchat.Channel, len(p.channels))
	copy(channels, p.channels)
	active := p.activeChannel
	p.dataMu.Unlock()

	p.channelList.Clear()
	currentIdx := 0
	for i, ch := range channels {
		p.channelList.AddItem("# "+ch.Name, "", 0, nil)
		if ch.ID == active {
			currentIdx = i
		}
	}
	if len(channels) > 0 {
		p.channelList.SetCurrentItem(currentIdx)
	}
	p.renderChannelTitle()
}

// renderChannelTitle sets the message view title to the active channel name.
func (p *TeamChatPage) renderChannelTitle() {
	p.dataMu.Lock()
	name := ""
	for _, ch := range p.channels {
		if ch.ID == p.activeChannel {
			name = ch.Name
			break
		}
	}
	p.dataMu.Unlock()
	if name == "" {
		p.msgView.SetTitle(" Team Chat ")
		return
	}
	p.msgView.SetTitle(" #" + tview.Escape(name) + " ")
}

// renderMessages redraws the full message buffer. Must run on the main
// goroutine.
func (p *TeamChatPage) renderMessages() {
	p.dataMu.Lock()
	messages := make([]teamchat.Message, len(p.messages))
	copy(messages, p.messages)
	p.dataMu.Unlock()

	p.msgView.Clear()
	if len(messages) == 0 {
		fmt.Fprint(p.msgView, " [gray]No messages yet — say hi![white]\n")
		return
	}
	for _, msg := range messages {
		p.writeMessageLine(msg)
	}
	p.msgView.ScrollToEnd()
}

// appendMessage renders a single message at the end of the view. Must run on
// the main goroutine.
func (p *TeamChatPage) appendMessage(msg teamchat.Message) {
	p.writeMessageLine(msg)
	p.msgView.ScrollToEnd()
}

// writeMessageLine renders one message into the view.
func (p *TeamChatPage) writeMessageLine(msg teamchat.Message) {
	fmt.Fprintf(p.msgView, " %s\n", formatTeamChatMessage(msg))
}

// renderMembers redraws the members sidebar. Must run on the main goroutine.
func (p *TeamChatPage) renderMembers(members []teamchat.Member) {
	if len(members) == 0 {
		p.membersBox.SetText(" [gray]no members[white]")
		return
	}
	var sb strings.Builder
	for _, m := range members {
		marker := "[white]"
		if m.Role == "admin" {
			marker = "[yellow]"
		}
		fmt.Fprintf(&sb, " %s%s[white]\n", marker, tview.Escape(m.Label()))
	}
	p.membersBox.SetText(sb.String())
}

// renderStatus updates the one-line status bar. Must run on the main
// goroutine (Build seeds it before the app runs, which is also safe).
func (p *TeamChatPage) renderStatus(state connState, detail string) {
	if p.statusBar == nil {
		return
	}
	p.configMu.Lock()
	base := p.cfg.APIBaseURL
	p.configMu.Unlock()
	p.statusBar.SetText(formatChatStatusBar(state, base, detail))
}

// formatTeamChatMessage renders a message as a single tview-markup line:
// timestamp, sender (agents in magenta, humans in cyan), thread marker, body.
// Pure and unit-testable.
func formatTeamChatMessage(msg teamchat.Message) string {
	ts := msg.CreatedAt.Local().Format("Jan 02 15:04")
	name := msg.SenderLabel()

	nameColor := "cyan"
	if msg.MessageType == "agent" {
		nameColor = "magenta"
	}

	thread := ""
	if msg.ParentMessageID != nil && *msg.ParentMessageID != "" {
		thread = "↳ "
	}

	body := strings.TrimSpace(msg.Content)
	if body == "" && len(msg.Attachments) > 0 {
		body = fmt.Sprintf("(%d attachment(s))", len(msg.Attachments))
	}

	line := fmt.Sprintf("[gray]%s[white] %s[%s]%s[white]: %s",
		ts, thread, nameColor, tview.Escape(name), tview.Escape(body))

	if msg.ReplyCount > 0 {
		line += fmt.Sprintf(" [gray](%d replies)[white]", msg.ReplyCount)
	}
	for _, att := range msg.Attachments {
		line += fmt.Sprintf("\n   [gray]+ attachment: %s[white]", tview.Escape(att.FileName))
	}
	return line
}

// parseSearchCommand recognizes "/search <query>" input. Pure and
// unit-testable. Returns the query and true when the input is a search
// command with a non-empty query.
func parseSearchCommand(text string) (string, bool) {
	const prefix = "/search"
	if !strings.HasPrefix(text, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	if rest == "" {
		return "", false
	}
	return rest, true
}

// newestMessageID returns the ID of the last (newest) message, or "".
// Messages arrive oldest-first from the API.
func newestMessageID(messages []teamchat.Message) string {
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].ID
}
