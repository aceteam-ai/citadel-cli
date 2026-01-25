package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// TviewDashboard is a rich interactive dashboard using tview
type TviewDashboard struct {
	app       *tview.Application
	data      StatusData
	refreshFn func() (StatusData, error)

	// UI components
	mainFlex    *tview.Flex
	nodeInfo    *tview.TextView
	vitals      *tview.TextView
	gpuInfo     *tview.TextView
	services    *tview.Table
	peers       *tview.Table
	statusBar   *tview.TextView
	autoRefresh bool
	stopChan    chan struct{}
}

// NewTviewDashboard creates a new tview-based dashboard
func NewTviewDashboard(data StatusData, refreshFn func() (StatusData, error)) *TviewDashboard {
	return &TviewDashboard{
		data:        data,
		refreshFn:   refreshFn,
		autoRefresh: true,
		stopChan:    make(chan struct{}),
	}
}

// Run starts the tview dashboard
func (d *TviewDashboard) Run() error {
	d.app = tview.NewApplication()
	d.buildUI()
	d.updateUI()

	// Start auto-refresh
	go d.autoRefreshLoop()

	// Set up key bindings
	d.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			d.stop()
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 'q', 'Q':
				d.stop()
				return nil
			case 'r', 'R':
				go d.refresh()
				return nil
			case 'a', 'A':
				d.autoRefresh = !d.autoRefresh
				d.updateStatusBar()
				return nil
			}
		}
		return event
	})

	return d.app.Run()
}

func (d *TviewDashboard) stop() {
	close(d.stopChan)
	d.app.Stop()
}

func (d *TviewDashboard) buildUI() {
	// Node info panel
	d.nodeInfo = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	d.nodeInfo.SetBorder(true).SetTitle(" Node ")

	// System vitals panel
	d.vitals = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	d.vitals.SetBorder(true).SetTitle(" System Vitals ")

	// GPU info panel
	d.gpuInfo = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	d.gpuInfo.SetBorder(true).SetTitle(" GPU ")

	// Services table
	d.services = tview.NewTable().
		SetBorders(false).
		SetSelectable(false, false)
	d.services.SetBorder(true).SetTitle(" Services ")

	// Peers table
	d.peers = tview.NewTable().
		SetBorders(false).
		SetSelectable(false, false)
	d.peers.SetBorder(true).SetTitle(" Network Peers ")

	// Status bar
	d.statusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	// Layout: top row (node + vitals), middle row (gpu + services), bottom (peers), status bar
	topRow := tview.NewFlex().
		AddItem(d.nodeInfo, 0, 1, false).
		AddItem(d.vitals, 0, 1, false)

	middleRow := tview.NewFlex().
		AddItem(d.gpuInfo, 0, 1, false).
		AddItem(d.services, 0, 1, false)

	d.mainFlex = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.createHeader(), 3, 0, false).
		AddItem(topRow, 8, 0, false).
		AddItem(middleRow, 10, 0, false).
		AddItem(d.peers, 0, 1, false).
		AddItem(d.statusBar, 1, 0, false)

	d.app.SetRoot(d.mainFlex, true)
}

func (d *TviewDashboard) createHeader() *tview.TextView {
	header := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)
	header.SetText(fmt.Sprintf("\n[::b]⚡ CITADEL STATUS[::-] [gray](%s)[-]", d.data.Version))
	return header
}

func (d *TviewDashboard) updateUI() {
	d.updateNodeInfo()
	d.updateVitals()
	d.updateGPU()
	d.updateServices()
	d.updatePeers()
	d.updateStatusBar()
}

func (d *TviewDashboard) updateNodeInfo() {
	var sb strings.Builder

	nodeName := d.data.NodeName
	if nodeName == "" {
		nodeName = "unknown"
	}
	nodeIP := d.data.NodeIP
	if nodeIP == "" {
		nodeIP = "-"
	}
	org := d.data.OrgID
	if org == "" {
		org = "-"
	}

	statusColor := "red"
	statusText := "OFFLINE"
	statusIcon := "●"
	if d.data.Connected {
		statusColor = "green"
		statusText = "ONLINE"
	}

	sb.WriteString(fmt.Sprintf(" [yellow]Node:[-]         %s\n", nodeName))
	sb.WriteString(fmt.Sprintf(" [yellow]IP:[-]           %s\n", nodeIP))
	sb.WriteString(fmt.Sprintf(" [yellow]Organization:[-] %s\n", org))
	if len(d.data.Tags) > 0 {
		sb.WriteString(fmt.Sprintf(" [yellow]Tags:[-]         %s\n", strings.Join(d.data.Tags, ", ")))
	}
	sb.WriteString(fmt.Sprintf(" [yellow]Status:[-]       [%s]%s %s[-]", statusColor, statusIcon, statusText))

	d.nodeInfo.SetText(sb.String())
}

func (d *TviewDashboard) updateVitals() {
	var sb strings.Builder

	sb.WriteString(d.formatProgressLine("CPU", d.data.CPUPercent, ""))
	sb.WriteString(d.formatProgressLine("Memory", d.data.MemoryPercent, fmt.Sprintf("%s / %s", d.data.MemoryUsed, d.data.MemoryTotal)))
	sb.WriteString(d.formatProgressLine("Disk", d.data.DiskPercent, fmt.Sprintf("%s / %s", d.data.DiskUsed, d.data.DiskTotal)))

	d.vitals.SetText(sb.String())
}

func (d *TviewDashboard) formatProgressLine(label string, percent float64, detail string) string {
	barWidth := 20
	filled := min(int(percent/100.0*float64(barWidth)), barWidth)
	empty := barWidth - filled

	color := "green"
	if percent >= 90 {
		color = "red"
	} else if percent >= 75 {
		color = "yellow"
	}

	bar := fmt.Sprintf("[%s]%s[-][gray]%s[-]", color, strings.Repeat("█", filled), strings.Repeat("░", empty))
	pct := fmt.Sprintf("[%s]%5.1f%%[-]", color, percent)

	line := fmt.Sprintf(" [yellow]%-8s[-] %s %s", label+":", bar, pct)
	if detail != "" {
		line += fmt.Sprintf(" [gray](%s)[-]", detail)
	}
	return line + "\n"
}

func (d *TviewDashboard) updateGPU() {
	var sb strings.Builder

	if len(d.data.GPUs) == 0 {
		sb.WriteString(" [gray]No GPU detected[-]")
	} else {
		for i, gpu := range d.data.GPUs {
			sb.WriteString(fmt.Sprintf(" [yellow]GPU %d:[-] %s\n", i, gpu.Name))
			if gpu.Memory != "" {
				sb.WriteString(fmt.Sprintf("   [gray]Memory:[-] %s\n", gpu.Memory))
			}
			if gpu.Temperature != "" {
				temp := gpu.Temperature
				color := "green"
				if strings.Contains(temp, "8") || strings.Contains(temp, "9") {
					color = "red"
				} else if strings.Contains(temp, "7") {
					color = "yellow"
				}
				sb.WriteString(fmt.Sprintf("   [gray]Temp:[-]   [%s]%s[-]\n", color, temp))
			}
			if gpu.Utilization > 0 {
				sb.WriteString(d.formatProgressLine("Util", gpu.Utilization, ""))
			}
			if gpu.Driver != "" {
				sb.WriteString(fmt.Sprintf("   [gray]Driver:[-] %s\n", gpu.Driver))
			}
		}
	}

	d.gpuInfo.SetText(sb.String())
}

func (d *TviewDashboard) updateServices() {
	d.services.Clear()

	if len(d.data.Services) == 0 {
		d.services.SetCell(0, 0, tview.NewTableCell(" [gray]No services configured[-]").SetSelectable(false))
		return
	}

	// Header
	headers := []string{"NAME", "STATUS", "UPTIME"}
	for i, h := range headers {
		cell := tview.NewTableCell(" [yellow::b]" + h + "[-:-:-]").
			SetSelectable(false).
			SetAlign(tview.AlignLeft)
		d.services.SetCell(0, i, cell)
	}

	// Data rows
	for i, svc := range d.data.Services {
		row := i + 1

		// Name
		d.services.SetCell(row, 0, tview.NewTableCell(" "+svc.Name).SetSelectable(false))

		// Status
		var statusCell string
		switch svc.Status {
		case "running":
			statusCell = "[green]● running[-]"
		case "stopped":
			statusCell = "[gray]○ stopped[-]"
		case "error":
			statusCell = "[red]✗ error[-]"
		default:
			statusCell = "[yellow]? " + svc.Status + "[-]"
		}
		d.services.SetCell(row, 1, tview.NewTableCell(statusCell).SetSelectable(false))

		// Uptime
		uptime := svc.Uptime
		if uptime == "" {
			uptime = "-"
		}
		d.services.SetCell(row, 2, tview.NewTableCell("[gray]"+uptime+"[-]").SetSelectable(false))
	}
}

func (d *TviewDashboard) updatePeers() {
	d.peers.Clear()

	if len(d.data.Peers) == 0 {
		d.peers.SetCell(0, 0, tview.NewTableCell(" [gray]No peers connected[-]").SetSelectable(false))
		return
	}

	// Header
	headers := []string{"STATUS", "HOSTNAME", "IP", "LATENCY", "TYPE"}
	for i, h := range headers {
		cell := tview.NewTableCell(" [yellow::b]" + h + "[-:-:-]").
			SetSelectable(false).
			SetAlign(tview.AlignLeft)
		d.peers.SetCell(0, i, cell)
	}

	// Data rows
	for i, peer := range d.data.Peers {
		row := i + 1

		// Status
		var statusIcon string
		if peer.Online {
			statusIcon = "[green]●[-]"
		} else {
			statusIcon = "[gray]○[-]"
		}
		d.peers.SetCell(row, 0, tview.NewTableCell(" "+statusIcon).SetSelectable(false))

		// Hostname
		d.peers.SetCell(row, 1, tview.NewTableCell(peer.Hostname).SetSelectable(false))

		// IP
		d.peers.SetCell(row, 2, tview.NewTableCell("[gray]"+peer.IP+"[-]").SetSelectable(false))

		// Latency
		latency := peer.Latency
		if latency == "" {
			latency = "-"
		}
		d.peers.SetCell(row, 3, tview.NewTableCell("[gray]"+latency+"[-]").SetSelectable(false))

		// Connection type
		connType := peer.ConnType
		if connType == "" {
			connType = "-"
		}
		d.peers.SetCell(row, 4, tview.NewTableCell("[gray]"+connType+"[-]").SetSelectable(false))
	}
}

func (d *TviewDashboard) updateStatusBar() {
	autoStr := "[red]off[-]"
	if d.autoRefresh {
		autoStr = "[green]on[-]"
	}

	lastUpdate := "never"
	if !d.data.LastUpdate.IsZero() {
		lastUpdate = d.data.LastUpdate.Format("15:04:05")
	}

	d.statusBar.SetText(fmt.Sprintf(
		" [yellow][r][-]efresh  [yellow][a][-]uto-refresh: %s  [yellow][q][-]uit  |  Last update: [gray]%s[-]",
		autoStr, lastUpdate,
	))
}

func (d *TviewDashboard) refresh() {
	if d.refreshFn == nil {
		return
	}

	data, err := d.refreshFn()
	if err != nil {
		return
	}

	d.data = data
	d.data.LastUpdate = time.Now()

	d.app.QueueUpdateDraw(func() {
		d.updateUI()
	})
}

func (d *TviewDashboard) autoRefreshLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopChan:
			return
		case <-ticker.C:
			if d.autoRefresh {
				d.refresh()
			}
		}
	}
}

// RunTviewDashboard runs the tview-based interactive status dashboard
func RunTviewDashboard(data StatusData, refreshFn func() (StatusData, error)) error {
	dashboard := NewTviewDashboard(data, refreshFn)
	return dashboard.Run()
}
