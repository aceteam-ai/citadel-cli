package controlcenter

import (
	"fmt"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/gateway"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// GatewayPage displays gateway metering stats and recent transactions.
// It reads data from the on-disk transaction ledger, so it works across
// processes (the gateway runs via `citadel gateway`, the TUI via `citadel`).
type GatewayPage struct {
	name  string
	title string
	app   *tview.Application

	// UI components
	root         *tview.Flex
	statusView   *tview.TextView
	statsView    *tview.TextView
	recentTable  *tview.Table
	endpointView *tview.TextView

	// Data
	ledger *gateway.Ledger
	mu     sync.Mutex
	stats  gateway.Stats
	recent []gateway.Transaction
	active bool
	stopCh chan struct{}
}

// NewGatewayPage creates a gateway page backed by a ledger at the given
// base directory (typically ~/.citadel-cli).
func NewGatewayPage(baseDir string) *GatewayPage {
	return &GatewayPage{
		name:   "gateway",
		title:  "Gateway",
		ledger: gateway.NewLedger(baseDir),
	}
}

func (g *GatewayPage) Name() string  { return g.name }
func (g *GatewayPage) Title() string { return g.title }

func (g *GatewayPage) Build(app *tview.Application) tview.Primitive {
	g.app = app

	// Status bar (top)
	g.statusView = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)

	// Stats panel
	g.statsView = tview.NewTextView().
		SetDynamicColors(true)
	g.statsView.SetBorder(true).
		SetTitle(" Earnings ").
		SetTitleAlign(tview.AlignLeft)

	// Recent requests table
	g.recentTable = tview.NewTable().
		SetFixed(1, 0).
		SetSelectable(true, false)
	g.recentTable.SetBorder(true).
		SetTitle(" Recent Requests ").
		SetTitleAlign(tview.AlignLeft)

	// Endpoint info (bottom)
	g.endpointView = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)

	// Layout
	g.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(g.statusView, 1, 0, false).
		AddItem(g.statsView, 7, 0, false).
		AddItem(g.recentTable, 0, 1, true).
		AddItem(g.endpointView, 1, 0, false)

	g.updateStatus()
	g.updateStats()
	g.updateRecentTable()
	g.updateEndpoint()

	return g.root
}

func (g *GatewayPage) OnActivate() {
	g.mu.Lock()
	g.active = true
	g.stopCh = make(chan struct{})
	g.mu.Unlock()

	// Start polling the ledger for updates
	go g.pollLoop()
}

func (g *GatewayPage) OnDeactivate() {
	g.mu.Lock()
	g.active = false
	if g.stopCh != nil {
		close(g.stopCh)
		g.stopCh = nil
	}
	g.mu.Unlock()
}

func (g *GatewayPage) HandleInput(event *tcell.EventKey) *tcell.EventKey {
	return event
}

// pollLoop reads the ledger from disk every 2 seconds and updates the UI.
func (g *GatewayPage) pollLoop() {
	g.refreshData()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		g.mu.Lock()
		stopCh := g.stopCh
		g.mu.Unlock()

		select {
		case <-stopCh:
			return
		case <-ticker.C:
			g.refreshData()
		}
	}
}

func (g *GatewayPage) refreshData() {
	stats, err := g.ledger.StatsFromDisk()
	if err != nil {
		return
	}
	recent, err := g.ledger.Recent(20)
	if err != nil {
		return
	}

	g.mu.Lock()
	g.stats = stats
	g.recent = recent
	g.mu.Unlock()

	g.app.QueueUpdateDraw(func() {
		g.updateStatus()
		g.updateStats()
		g.updateRecentTable()
	})
}

func (g *GatewayPage) updateStatus() {
	g.mu.Lock()
	s := g.stats
	g.mu.Unlock()

	if s.TotalRequests > 0 {
		g.statusView.SetText(" [green::b]Gateway[-:-:-]                    [green]● Active[-]")
	} else {
		g.statusView.SetText(" [yellow::b]Gateway[-:-:-]                    [gray]○ No activity[-]")
	}
}

func (g *GatewayPage) updateStats() {
	g.mu.Lock()
	s := g.stats
	g.mu.Unlock()

	todayDollars := float64(s.TodayEarnings) * 0.001
	totalDollars := float64(s.TotalEarnings) * 0.001
	avgLatency := s.AvgLatency

	text := fmt.Sprintf(
		" [white::b]Earnings Today[white:-:-]    [green]▲ %d ACET[-]  ($%.3f)\n"+
			" [white::b]Total Earnings[white:-:-]      %d ACET  ($%.2f)\n"+
			" [white::b]Requests Today[white:-:-]      %d\n"+
			" [white::b]Total Requests[white:-:-]      %d\n"+
			" [white::b]Avg Latency[white:-:-]         %.0fms",
		s.TodayEarnings, todayDollars,
		s.TotalEarnings, totalDollars,
		s.TodayRequests,
		s.TotalRequests,
		avgLatency,
	)
	g.statsView.SetText(text)
}

func (g *GatewayPage) updateRecentTable() {
	g.mu.Lock()
	recent := g.recent
	g.mu.Unlock()

	g.recentTable.Clear()

	// Header row
	headers := []string{"Time", "Method", "Path", "Model", "Tokens", "ACET", "Latency"}
	for i, h := range headers {
		cell := tview.NewTableCell(h).
			SetTextColor(tcell.ColorYellow).
			SetSelectable(false).
			SetExpansion(1)
		if i == 0 {
			cell.SetExpansion(0)
		}
		g.recentTable.SetCell(0, i, cell)
	}

	if len(recent) == 0 {
		g.recentTable.SetCell(1, 0,
			tview.NewTableCell("  No requests yet").
				SetTextColor(tcell.ColorGray).
				SetSelectable(false).
				SetExpansion(1))
		return
	}

	for i, tx := range recent {
		row := i + 1
		timeStr := tx.Timestamp.Format("15:04:05")
		tokenStr := fmt.Sprintf("%d/%d", tx.TokensIn, tx.TokensOut)
		acetStr := fmt.Sprintf("%d", tx.ACETCost)
		latencyStr := fmt.Sprintf("%.0fms", tx.Latency)

		g.recentTable.SetCell(row, 0, tview.NewTableCell(timeStr).SetTextColor(tcell.ColorWhite))
		g.recentTable.SetCell(row, 1, tview.NewTableCell("POST").SetTextColor(tcell.ColorWhite))
		g.recentTable.SetCell(row, 2, tview.NewTableCell(tx.Path).SetTextColor(tcell.ColorWhite).SetExpansion(1))
		g.recentTable.SetCell(row, 3, tview.NewTableCell(tx.Model).SetTextColor(tcell.ColorAqua).SetExpansion(1))
		g.recentTable.SetCell(row, 4, tview.NewTableCell(tokenStr).SetTextColor(tcell.ColorWhite))
		g.recentTable.SetCell(row, 5, tview.NewTableCell(acetStr).SetTextColor(tcell.ColorGreen))
		g.recentTable.SetCell(row, 6, tview.NewTableCell(latencyStr).SetTextColor(tcell.ColorWhite))
	}
}

func (g *GatewayPage) updateEndpoint() {
	g.endpointView.SetText(" [gray]Endpoint: http://localhost:8080/v1  |  Start with: citadel gateway[-]")
}
