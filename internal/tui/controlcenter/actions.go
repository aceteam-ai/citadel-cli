// Package controlcenter provides action implementations for the Control Center TUI.
package controlcenter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)


// showListModal displays a modal with a list of items and calls onSelect when one is chosen.
func (cc *ControlCenter) showListModal(title string, items []string, onSelect func(selected string)) {
	if len(items) == 0 {
		cc.AddActivity("warning", "No items available")
		return
	}

	cc.inModal = true

	list := tview.NewList()
	list.SetBorder(true).SetTitle(" " + title + " ")

	for i, item := range items {
		idx := i
		itemCopy := item
		list.AddItem(item, "", rune('a'+idx), func() {
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.servicesView)
			if onSelect != nil {
				onSelect(itemCopy)
			}
		})
	}

	// Add cancel option
	list.AddItem("Cancel", "", 'q', func() {
		cc.inModal = false
		cc.app.SetRoot(cc.mainFlex, true)
		cc.app.SetFocus(cc.servicesView)
	})

	// Handle escape key
	list.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.servicesView)
			return nil
		}
		return event
	})

	cc.app.SetRoot(list, true)
	cc.app.SetFocus(list)
}

// showInfoModal displays an info modal with the given content.
func (cc *ControlCenter) showInfoModal(title, content string) {
	cc.inModal = true

	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetText(content)
	textView.SetBorder(true).SetTitle(" " + title + " ")

	textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		cc.inModal = false
		cc.app.SetRoot(cc.mainFlex, true)
		cc.app.SetFocus(cc.servicesView)
		return nil
	})

	cc.app.SetRoot(textView, true)
	cc.app.SetFocus(textView)
}

// showServiceDetailModal displays a modal with service details and actions
func (cc *ControlCenter) showServiceDetailModal() {
	svcName := cc.getSelectedServiceName()
	if svcName == "" {
		cc.AddActivity("info", "No service selected")
		return
	}

	cc.inModal = true

	// Find service info
	var svcInfo *ServiceInfo
	for i := range cc.data.Services {
		if cc.data.Services[i].Name == svcName {
			svcInfo = &cc.data.Services[i]
			break
		}
	}

	if svcInfo == nil {
		cc.inModal = false
		cc.AddActivity("error", "Service not found")
		return
	}

	// Build content
	updateContent := func(svc *ServiceInfo) string {
		var sb strings.Builder

		sb.WriteString(fmt.Sprintf("[yellow::b]Service: %s[-:-:-]\n\n", svc.Name))

		// Status with icon
		switch svc.Status {
		case "running":
			sb.WriteString("[green]● Status: running[-]\n")
		case "stopped":
			sb.WriteString("[gray]○ Status: stopped[-]\n")
		case "error":
			sb.WriteString("[red]✗ Status: error[-]\n")
		default:
			sb.WriteString(fmt.Sprintf("[yellow]? Status: %s[-]\n", svc.Status))
		}

		// Uptime
		if svc.Uptime != "" {
			sb.WriteString(fmt.Sprintf("[yellow]Uptime:[-] %s\n", svc.Uptime))
		}

		// Container info if running (get via docker inspect)
		if svc.Status == "running" && cc.getServiceDetailFn != nil {
			if detail := cc.getServiceDetailFn(svc.Name); detail != nil {
				if detail.ContainerID != "" {
					sb.WriteString(fmt.Sprintf("[yellow]Container:[-] %s\n", detail.ContainerID))
				}
				if detail.Image != "" {
					sb.WriteString(fmt.Sprintf("[yellow]Image:[-] %s\n", detail.Image))
				}
				if detail.ComposePath != "" {
					sb.WriteString(fmt.Sprintf("[yellow]Compose:[-] %s\n", detail.ComposePath))
				}
				if len(detail.Ports) > 0 {
					sb.WriteString(fmt.Sprintf("[yellow]Ports:[-] %s\n", strings.Join(detail.Ports, ", ")))
				}
			}
		}

		// Actions hint
		sb.WriteString("\n[gray]─────────────────────────────────────────[-]\n")
		if svc.Status == "running" {
			sb.WriteString("[yellow]X[-] Stop  │  [yellow]R[-] Restart  │  [yellow]L[-] Logs  │  [gray]Esc[-] Close")
		} else {
			sb.WriteString("[yellow]S[-] Start  │  [yellow]L[-] Logs  │  [gray]Esc[-] Close")
		}

		return sb.String()
	}

	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	textView.SetText(updateContent(svcInfo))
	textView.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", svcName))

	closeModal := func() {
		cc.inModal = false
		cc.app.SetRoot(cc.mainFlex, true)
		cc.updatePaneFocus()
	}

	textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			closeModal()
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 's', 'S':
				if svcInfo.Status != "running" {
					closeModal()
					cc.startSelectedService()
				}
				return nil
			case 'x', 'X':
				if svcInfo.Status == "running" {
					closeModal()
					cc.stopSelectedService()
				}
				return nil
			case 'r', 'R':
				if svcInfo.Status == "running" {
					closeModal()
					cc.restartSelectedService()
				}
				return nil
			case 'l', 'L':
				closeModal()
				cc.showServiceLogs(svcName)
				return nil
			}
		}
		return event
	})

	cc.app.SetRoot(textView, true)
	cc.app.SetFocus(textView)
}

// showAddServiceModal shows available services to add (Action 1)
func (cc *ControlCenter) showAddServiceModal() {
	if cc.getServicesFn == nil {
		cc.AddActivity("error", "Service management not available")
		return
	}

	available := cc.getServicesFn()
	if len(available) == 0 {
		cc.AddActivity("warning", "No services available to add")
		return
	}

	// Filter out already configured services
	var configured map[string]bool
	if cc.getConfiguredFn != nil {
		configuredList := cc.getConfiguredFn()
		configured = make(map[string]bool, len(configuredList))
		for _, svc := range configuredList {
			configured[svc] = true
		}
	}

	var toAdd []string
	for _, svc := range available {
		if configured == nil || !configured[svc] {
			toAdd = append(toAdd, svc)
		}
	}

	if len(toAdd) == 0 {
		cc.AddActivity("info", "All available services are already configured")
		return
	}

	cc.showListModal("Add Service", toAdd, func(selected string) {
		if cc.addServiceFn == nil {
			cc.AddActivity("error", "Cannot add service: no handler configured")
			return
		}

		go func() {
			cc.AddActivity("info", fmt.Sprintf("Adding service: %s...", selected))
			if err := cc.addServiceFn(selected); err != nil {
				cc.AddActivity("error", fmt.Sprintf("Failed to add %s: %v", selected, err))
			} else {
				cc.AddActivity("success", fmt.Sprintf("Service %s added", selected))
				cc.refresh()
			}
		}()
	})
}

// showExposePortModal shows a form to expose a local port (Action 2)
func (cc *ControlCenter) showExposePortModal() {
	if !cc.data.Connected {
		cc.AddActivity("error", "Not connected to AceTeam Network - press 0 to connect")
		return
	}

	cc.inModal = true

	// Use a simple input field approach instead of Form widget
	portInput := tview.NewInputField().
		SetLabel("Local Port: ").
		SetFieldWidth(10).
		SetAcceptanceFunc(tview.InputFieldInteger)

	descInput := tview.NewInputField().
		SetLabel("Description: ").
		SetFieldWidth(30)

	// Create a flex layout
	formFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 1, 0, false).
		AddItem(portInput, 1, 0, true).
		AddItem(nil, 1, 0, false).
		AddItem(descInput, 1, 0, false).
		AddItem(nil, 1, 0, false).
		AddItem(tview.NewTextView().SetText("[yellow]Tab[-] switch field  [yellow]Enter[-] expose  [yellow]Esc[-] cancel").SetDynamicColors(true).SetTextAlign(tview.AlignCenter), 1, 0, false)

	formFlex.SetBorder(true).SetTitle(" Expose Port ")

	// Center the form
	centered := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(formFlex, 10, 0, true).
			AddItem(nil, 0, 1, false), 50, 0, true).
		AddItem(nil, 0, 1, false)

	currentField := 0 // 0 = port, 1 = description

	closeModal := func() {
		cc.inModal = false
		cc.app.SetRoot(cc.mainFlex, true)
		cc.updatePaneFocus()
	}

	submitForm := func() {
		portStr := portInput.GetText()
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			cc.AddActivity("error", "Invalid port number (1-65535)")
			return
		}

		description := descInput.GetText()
		closeModal()
		go cc.exposePort(port, description)
	}

	// Input handling for both fields
	handleInput := func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			closeModal()
			return nil
		case tcell.KeyEnter:
			submitForm()
			return nil
		case tcell.KeyTab, tcell.KeyDown:
			if currentField == 0 {
				currentField = 1
				cc.app.SetFocus(descInput)
			} else {
				currentField = 0
				cc.app.SetFocus(portInput)
			}
			return nil
		case tcell.KeyBacktab, tcell.KeyUp:
			if currentField == 1 {
				currentField = 0
				cc.app.SetFocus(portInput)
			} else {
				currentField = 1
				cc.app.SetFocus(descInput)
			}
			return nil
		}
		return event
	}

	portInput.SetInputCapture(handleInput)
	descInput.SetInputCapture(handleInput)

	cc.app.SetRoot(centered, true)
	cc.app.SetFocus(portInput)
}

// exposePort creates a listener on the AceTeam network for the given port.
func (cc *ControlCenter) exposePort(port int, description string) {
	cc.AddActivity("info", fmt.Sprintf("Exposing port %d...", port))

	// Create a listener on the network
	addr := fmt.Sprintf(":%d", port)
	listener, err := network.Listen("tcp", addr)
	if err != nil {
		cc.AddActivity("error", fmt.Sprintf("Failed to expose port %d: %v", port, err))
		return
	}

	// Track the forward
	forward := PortForward{
		LocalPort:   port,
		Description: description,
		Listener:    listener,
		StartedAt:   time.Now(),
	}
	cc.activeForwards = append(cc.activeForwards, forward)

	desc := description
	if desc == "" {
		desc = "port forward"
	}
	cc.AddActivity("success", fmt.Sprintf("Port %d exposed (%s)", port, desc))

	// Start accepting connections and forwarding to local port
	go cc.handlePortForward(listener, port)
}

// handlePortForward accepts connections on the network listener and forwards to localhost.
func (cc *ControlCenter) handlePortForward(listener net.Listener, localPort int) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Listener was closed
			return
		}

		go func(c net.Conn) {
			defer c.Close()

			// Connect to local port
			localConn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", localPort))
			if err != nil {
				cc.AddActivity("warning", fmt.Sprintf("Failed to connect to localhost:%d", localPort))
				return
			}
			defer localConn.Close()

			// Bidirectional copy
			done := make(chan struct{})
			go func() {
				copyData(localConn, c)
				done <- struct{}{}
			}()
			copyData(c, localConn)
			<-done
		}(conn)
	}
}

// copyData copies data between connections (helper for port forwarding).
func copyData(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// showPortForwardsModal shows active port forwards (Action 3)
func (cc *ControlCenter) showPortForwardsModal() {
	if len(cc.activeForwards) == 0 {
		cc.AddActivity("info", "No active port forwards")
		return
	}

	cc.inModal = true

	table := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	table.SetBorder(true).SetTitle(" Port Forwards (d=delete, Esc=close) ")

	// Header
	headers := []string{"PORT", "DESCRIPTION", "STARTED", "DURATION"}
	for i, h := range headers {
		cell := tview.NewTableCell("[yellow::b]" + h + "[-:-:-]").SetSelectable(false)
		table.SetCell(0, i, cell)
	}

	// Rows
	for i, fwd := range cc.activeForwards {
		row := i + 1
		table.SetCell(row, 0, tview.NewTableCell(fmt.Sprintf("%d", fwd.LocalPort)))

		desc := fwd.Description
		if desc == "" {
			desc = "-"
		}
		table.SetCell(row, 1, tview.NewTableCell(desc))
		table.SetCell(row, 2, tview.NewTableCell(fwd.StartedAt.Format("15:04:05")))
		table.SetCell(row, 3, tview.NewTableCell(formatDuration(time.Since(fwd.StartedAt))))
	}

	table.Select(1, 0)

	table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.servicesView)
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'd' || event.Rune() == 'D' {
				row, _ := table.GetSelection()
				if row > 0 && row <= len(cc.activeForwards) {
					idx := row - 1
					fwd := cc.activeForwards[idx]

					// Close the listener
					if listener, ok := fwd.Listener.(net.Listener); ok {
						listener.Close()
					}

					// Remove from list
					cc.activeForwards = append(cc.activeForwards[:idx], cc.activeForwards[idx+1:]...)
					cc.AddActivity("info", fmt.Sprintf("Closed port forward on port %d", fwd.LocalPort))

					// Refresh the modal or close if empty
					if len(cc.activeForwards) == 0 {
						cc.inModal = false
						cc.app.SetRoot(cc.mainFlex, true)
						cc.app.SetFocus(cc.servicesView)
					} else {
						cc.showPortForwardsModal()
					}
				}
				return nil
			}
		}
		return event
	})

	cc.app.SetRoot(table, true)
	cc.app.SetFocus(table)
}

// formatDuration formats a duration for display.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// showSSHAccessModal shows SSH access status and instructions (Action 4)
func (cc *ControlCenter) showSSHAccessModal() {
	var sb strings.Builder

	// Check if SSH server is running locally
	sshRunning := isSSHServerRunning()

	if sshRunning {
		sb.WriteString("[green::b]SSH Status: Enabled[-:-:-]\n\n")
	} else {
		sb.WriteString("[yellow::b]SSH Status: Not Running[-:-:-]\n\n")
	}

	if cc.data.Connected && cc.data.NodeIP != "" {
		sb.WriteString("[yellow]Your node is accessible via:[-]\n")
		sb.WriteString(fmt.Sprintf("  [white]ssh user@%s[-]\n\n", cc.data.NodeIP))
	} else if !cc.data.Connected {
		sb.WriteString("[gray]Connect to AceTeam Network to enable remote SSH access.[-]\n\n")
	}

	if !sshRunning {
		sb.WriteString("[yellow]To enable SSH access:[-]\n")
		switch runtime.GOOS {
		case "linux":
			sb.WriteString("  [gray]sudo apt install openssh-server[-]\n")
			sb.WriteString("  [gray]sudo systemctl enable --now ssh[-]\n")
		case "darwin":
			sb.WriteString("  [gray]System Preferences > Sharing > Remote Login[-]\n")
		case "windows":
			sb.WriteString("  [gray]Settings > Apps > Optional Features > OpenSSH Server[-]\n")
		}
	}

	sb.WriteString("\n[gray]Press any key to close[-]")

	cc.showInfoModal("SSH Access", sb.String())
}

// isSSHServerRunning checks if an SSH server is listening on port 22.
func isSSHServerRunning() bool {
	conn, err := net.DialTimeout("tcp", "localhost:22", 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// pingPeers pings all peers and logs the results (Action 5)
func (cc *ControlCenter) pingPeers() {
	if !cc.data.Connected {
		cc.AddActivity("error", "Not connected to AceTeam Network - press 0 to connect")
		return
	}

	go func() {
		cc.AddActivity("info", "Pinging peers...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		peers, err := network.GetGlobalPeers(ctx)
		if err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to get peers: %v", err))
			return
		}

		if len(peers) == 0 {
			cc.AddActivity("info", "No peers to ping")
			return
		}

		for _, peer := range peers {
			if !peer.Online || peer.IP == "" {
				continue
			}

			latency, connType, relay, err := network.PingPeer(ctx, peer.IP)
			if err != nil {
				cc.AddActivity("warning", fmt.Sprintf("%s: unreachable", peer.Hostname))
				continue
			}

			connInfo := connType
			if relay != "" {
				connInfo = fmt.Sprintf("relay via %s", relay)
			}

			cc.AddActivity("success", fmt.Sprintf("%s: %.1fms (%s)", peer.Hostname, latency, connInfo))
		}

		cc.AddActivity("info", "Ping complete")
	}()
}

// showInstallServiceModal shows system service installation options (Action 6)
func (cc *ControlCenter) showInstallServiceModal() {
	cc.inModal = true

	// Check if already installed
	installed := isServiceInstalled()

	if installed {
		// Already installed - show status and management options
		var sb strings.Builder
		sb.WriteString("[green::b]Status: Citadel is installed as a system service[-:-:-]\n\n")

		switch runtime.GOOS {
		case "linux":
			sb.WriteString("[yellow]Management commands:[-]\n")
			sb.WriteString("  [gray]sudo systemctl status citadel[-]\n")
			sb.WriteString("  [gray]sudo systemctl restart citadel[-]\n")
			sb.WriteString("  [gray]sudo systemctl stop citadel[-]\n")
			sb.WriteString("  [gray]sudo citadel service uninstall[-]\n")
		case "darwin":
			sb.WriteString("[yellow]Management commands:[-]\n")
			sb.WriteString("  [gray]launchctl list | grep citadel[-]\n")
			sb.WriteString("  [gray]citadel service uninstall[-]\n")
		case "windows":
			sb.WriteString("[yellow]Management commands:[-]\n")
			sb.WriteString("  [gray]sc query citadel[-]\n")
			sb.WriteString("  [gray]citadel service uninstall[-]  (Run as Administrator)\n")
		}

		sb.WriteString("\n[gray]Press any key to close[-]")
		cc.showInfoModal("Install Service", sb.String())
		return
	}

	// Not installed - show confirmation to install
	var description string
	switch runtime.GOOS {
	case "linux":
		description = "This will create a systemd service that starts Citadel on boot."
	case "darwin":
		description = "This will create a launchd service that starts Citadel on boot."
	case "windows":
		description = "This will create a Windows Service that starts Citadel on boot.\n\nNote: Requires Administrator privileges."
	default:
		cc.inModal = false
		cc.AddActivity("warning", "System service installation not supported on this platform")
		return
	}

	modal := tview.NewModal().
		SetText(fmt.Sprintf("[yellow::b]Install Citadel as a system service?[-:-:-]\n\n%s\n\nThis will keep Citadel running in the background.", description)).
		AddButtons([]string{"Cancel", "Install Now"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.inModal = false
			if buttonLabel == "Install Now" {
				go cc.executeServiceInstall()
			}
			cc.app.SetRoot(cc.mainFlex, true)
			cc.updatePaneFocus()
		})

	cc.app.SetRoot(modal, true)
	cc.app.SetFocus(modal)
}

// executeServiceInstall runs the service installation command
func (cc *ControlCenter) executeServiceInstall() {
	cc.AddActivity("info", "Installing Citadel service...")

	// Find citadel binary path
	exePath, err := os.Executable()
	if err != nil {
		cc.AddActivity("error", fmt.Sprintf("Failed to find executable: %v", err))
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		// Requires sudo on Linux
		cmd = exec.Command("sudo", exePath, "service", "install")
	case "darwin":
		cmd = exec.Command(exePath, "service", "install")
	case "windows":
		cmd = exec.Command(exePath, "service", "install")
	default:
		cc.AddActivity("error", "Platform not supported")
		return
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		cc.AddActivity("error", fmt.Sprintf("Install failed: %v", err))
		if len(output) > 0 {
			// Log first line of output
			lines := strings.Split(string(output), "\n")
			if len(lines) > 0 && lines[0] != "" {
				cc.AddActivity("error", lines[0])
			}
		}
		return
	}

	cc.AddActivity("success", "Citadel service installed successfully")
	cc.AddActivity("info", "Service will start automatically on boot")
}

// isServiceInstalled checks if citadel is installed as a system service.
func isServiceInstalled() bool {
	switch runtime.GOOS {
	case "linux":
		cmd := exec.Command("systemctl", "is-enabled", "citadel")
		return cmd.Run() == nil
	case "darwin":
		cmd := exec.Command("launchctl", "list", "ai.aceteam.citadel")
		return cmd.Run() == nil
	case "windows":
		cmd := exec.Command("sc", "query", "citadel")
		return cmd.Run() == nil
	}
	return false
}


// showNetworkModal shows connect or disconnect options based on current state (Action 0)
func (cc *ControlCenter) showNetworkModal() {
	if cc.data.Connected {
		cc.showDisconnectConfirmModal()
	} else {
		cc.showConnectModal()
	}
}

// showConnectModal checks for existing tailscale connection and shows appropriate options
func (cc *ControlCenter) showConnectModal() {
	// Check if already connected via system tailscale to same network
	if cc.data.SystemTailscaleRunning && cc.data.DualConnection {
		cc.showAlreadyConnectedModal()
		return
	}

	// Check if system tailscale is on the same headscale but citadel isn't connected yet
	running, ip, name, sameNetwork := DetectSystemTailscale(cc.nexusURL)
	if running && sameNetwork {
		cc.showTailscaleDetectedModal(ip, name)
		return
	}

	// Normal flow - start device auth
	cc.startDeviceAuthFlow()
}

// ShowLoginPrompt shows a modal prompting the user to log in if not connected.
// Returns true if the prompt was shown, false if already connected.
func (cc *ControlCenter) ShowLoginPrompt() bool {
	if cc.data.Connected {
		return false
	}

	cc.inModal = true

	modal := tview.NewModal().
		SetText(`[yellow::b]Not Connected[-:-:-]

You're not connected to the AceTeam Network.

Connect to enable:
• Remote access to your services
• Network-wide port forwarding
• Peer-to-peer connectivity
• Job queue processing

Log in now?`).
		AddButtons([]string{"Log In", "Continue Offline"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.inModal = false
			if buttonLabel == "Log In" {
				cc.startDeviceAuthFlow()
			} else {
				cc.app.SetRoot(cc.mainFlex, true)
				cc.updatePaneFocus()
			}
		})

	cc.app.SetRoot(modal, true)
	cc.app.SetFocus(modal)
	return true
}

// QueueShowLoginPrompt safely queues the login prompt to be shown on the main thread.
// Use this when calling from a goroutine.
func (cc *ControlCenter) QueueShowLoginPrompt() {
	if cc.app == nil {
		return
	}
	cc.app.QueueUpdateDraw(func() {
		cc.ShowLoginPrompt()
	})
}

// showAlreadyConnectedModal explains dual connection status
func (cc *ControlCenter) showAlreadyConnectedModal() {
	var sb strings.Builder

	sb.WriteString("[green::b]Already Connected[-:-:-]\n\n")
	sb.WriteString("You have both connections active:\n\n")

	sb.WriteString("[yellow]System Tailscale:[-]\n")
	sb.WriteString(fmt.Sprintf("  %s (%s)\n", cc.data.SystemTailscaleName, cc.data.SystemTailscaleIP))
	sb.WriteString("  [gray]System-wide VPN, requires root[-]\n\n")

	sb.WriteString("[yellow]Citadel (embedded):[-]\n")
	sb.WriteString(fmt.Sprintf("  %s (%s)\n", cc.data.NodeName, cc.data.NodeIP))
	sb.WriteString("  [gray]App-specific, userspace networking[-]\n\n")

	sb.WriteString("[cyan]Both can coexist and reach each other on the mesh.[-]\n")
	sb.WriteString("[gray]Services exposed via Citadel use the Citadel IP.[-]\n")

	sb.WriteString("\n[gray]Press any key to close[-]")

	cc.showInfoModal("Network Status", sb.String())
}

// showTailscaleDetectedModal shows when system tailscale is already on the network
func (cc *ControlCenter) showTailscaleDetectedModal(tsIP, tsName string) {
	cc.inModal = true

	content := fmt.Sprintf(`[yellow::b]Tailscale Already Connected[-:-:-]

Your system Tailscale is already connected to the same network:

[yellow]System Tailscale:[-]
  %s (%s)

[cyan]Do you want to also connect via Citadel?[-]

[yellow]Differences:[-]
  [white]System Tailscale[-]
    • System-wide VPN (all apps use it)
    • Requires root/admin to install
    • Managed via 'tailscale' CLI

  [white]Citadel (embedded)[-]
    • App-specific (only Citadel services)
    • No root required (userspace networking)
    • Managed via this TUI
    • Separate identity on the mesh

[gray]Both can run simultaneously and reach each other.[-]`, tsName, tsIP)

	modal := tview.NewModal().
		SetText(content).
		AddButtons([]string{"Cancel", "Connect Citadel Too"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.inModal = false
			if buttonLabel == "Connect Citadel Too" {
				cc.startDeviceAuthFlow()
			} else {
				cc.app.SetRoot(cc.mainFlex, true)
				cc.updatePaneFocus()
			}
		})

	cc.app.SetRoot(modal, true)
	cc.app.SetFocus(modal)
}

// showDisconnectConfirmModal shows confirmation before disconnecting
func (cc *ControlCenter) showDisconnectConfirmModal() {
	cc.inModal = true

	nodeInfo := ""
	if cc.data.NodeName != "" {
		nodeInfo = fmt.Sprintf("\nNode: %s", cc.data.NodeName)
	}
	if cc.data.NodeIP != "" {
		nodeInfo += fmt.Sprintf("\nIP: %s", cc.data.NodeIP)
	}

	warningText := fmt.Sprintf(`[red::b]Disconnect from AceTeam Network?[-:-:-]
%s

[yellow]Warning:[-]
• Your services will no longer be accessible
• Other nodes won't be able to connect to this machine
• Active port forwards will be closed

Are you sure you want to disconnect?`, nodeInfo)

	modal := tview.NewModal().
		SetText(warningText).
		AddButtons([]string{"Cancel", "Disconnect"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			cc.inModal = false
			if buttonLabel == "Disconnect" {
				go cc.disconnectFromNetwork()
			}
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.servicesView)
		})

	cc.app.SetRoot(modal, true)
	cc.app.SetFocus(modal)
}

// disconnectFromNetwork disconnects from the AceTeam network
func (cc *ControlCenter) disconnectFromNetwork() {
	cc.AddActivity("info", "Disconnecting from network...")

	if cc.deviceAuth.Disconnect == nil {
		cc.AddActivity("error", "Disconnect not available")
		return
	}

	if err := cc.deviceAuth.Disconnect(); err != nil {
		cc.AddActivity("error", fmt.Sprintf("Failed to disconnect: %v", err))
		return
	}

	// Close all active port forwards
	for _, fwd := range cc.activeForwards {
		if listener, ok := fwd.Listener.(net.Listener); ok {
			listener.Close()
		}
	}
	cc.activeForwards = nil

	cc.AddActivity("success", "Disconnected from network")
	cc.refresh()
}

// startDeviceAuthFlow starts the device authorization flow
func (cc *ControlCenter) startDeviceAuthFlow() {
	if cc.deviceAuth.StartFlow == nil {
		cc.AddActivity("error", "Device authorization not available")
		return
	}

	go func() {
		cc.AddActivity("info", "Starting device authorization...")

		// Start the flow to get device code
		authConfig, err := cc.deviceAuth.StartFlow()
		if err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to start auth flow: %v", err))
			return
		}

		// Show the device auth modal
		cc.app.QueueUpdateDraw(func() {
			cc.showDeviceAuthModal(authConfig)
		})
	}()
}

// showDeviceAuthModal displays the device authorization code and polls for completion
func (cc *ControlCenter) showDeviceAuthModal(config *DeviceAuthConfig) {
	cc.inModal = true

	// Create a flex layout for the modal
	flex := tview.NewFlex().SetDirection(tview.FlexRow)

	// Content view
	contentView := tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignCenter)

	// Calculate expiry
	expiresAt := time.Now().Add(time.Duration(config.ExpiresIn) * time.Second)

	// Build complete URL with code
	completeURL := config.VerificationURI
	if !strings.Contains(completeURL, "?") {
		completeURL += "?code=" + config.UserCode
	}

	// Calculate box width based on code length
	codeLen := len(config.UserCode)
	boxWidth := codeLen + 6 // 3 spaces padding on each side
	topBottom := strings.Repeat("═", boxWidth)

	// Update function for countdown
	updateContent := func(status string) {
		var sb strings.Builder
		sb.WriteString("\n[yellow::b]Device Authorization[-:-:-]\n\n")
		sb.WriteString("Open this URL in your browser:\n\n")
		sb.WriteString(fmt.Sprintf("[white::b]%s[-:-:-]\n\n", completeURL))
		sb.WriteString("Or enter this code manually:\n\n")
		sb.WriteString(fmt.Sprintf("   [cyan::b]╔%s╗[-:-:-]\n", topBottom))
		sb.WriteString(fmt.Sprintf("   [cyan::b]║   %s   ║[-:-:-]\n", config.UserCode))
		sb.WriteString(fmt.Sprintf("   [cyan::b]╚%s╝[-:-:-]\n\n", topBottom))

		if status == "waiting" {
			remaining := time.Until(expiresAt)
			if remaining < 0 {
				remaining = 0
			}
			minutes := int(remaining.Minutes())
			seconds := int(remaining.Seconds()) % 60
			sb.WriteString(fmt.Sprintf("[gray]Waiting for authorization... (%d:%02d remaining)[-]\n", minutes, seconds))
		} else if status == "success" {
			sb.WriteString("[green::b]Authorization successful![-:-:-]\n")
		} else if strings.HasPrefix(status, "error:") {
			errMsg := strings.TrimPrefix(status, "error:")
			sb.WriteString(fmt.Sprintf("[red]%s[-]\n", errMsg))
		}

		sb.WriteString("\n[yellow]B[-] open browser  [yellow]C[-] copy link  [gray]Esc[-] cancel")
		contentView.SetText(sb.String())
	}

	updateContent("waiting")
	contentView.SetBorder(true).SetTitle(" Connect to AceTeam Network ")

	flex.AddItem(contentView, 0, 1, true)

	// Channel to signal completion
	doneChan := make(chan struct{})
	var pollCancel context.CancelFunc

	// Input capture for escape, B, and C keys
	contentView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			if pollCancel != nil {
				pollCancel()
			}
			close(doneChan)
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.app.SetFocus(cc.servicesView)
			cc.AddActivity("info", "Authorization canceled")
			return nil
		}
		if event.Key() == tcell.KeyRune {
			switch event.Rune() {
			case 'b', 'B':
				// Open browser with the verification URL
				if err := platform.OpenURL(completeURL); err != nil {
					cc.AddActivity("warning", fmt.Sprintf("Failed to open browser: %v", err))
				} else {
					cc.AddActivity("info", "Opened browser")
				}
				return nil
			case 'c', 'C':
				// Copy URL to clipboard
				if err := platform.CopyToClipboard(completeURL); err != nil {
					cc.AddActivity("warning", fmt.Sprintf("Failed to copy: %v", err))
				} else {
					cc.AddActivity("success", "Link copied to clipboard")
				}
				return nil
			}
		}
		return event
	})

	cc.app.SetRoot(flex, true)
	cc.app.SetFocus(contentView)

	// Start polling in background
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		pollCancel = cancel
		defer cancel()

		// Start countdown ticker
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		// Poll interval
		pollInterval := time.Duration(config.Interval) * time.Second
		if pollInterval < 5*time.Second {
			pollInterval = 5 * time.Second
		}
		pollTicker := time.NewTicker(pollInterval)
		defer pollTicker.Stop()

		for {
			select {
			case <-doneChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Update countdown
				cc.app.QueueUpdateDraw(func() {
					if cc.inModal {
						updateContent("waiting")
					}
				})
				// Check if expired
				if time.Now().After(expiresAt) {
					cc.app.QueueUpdateDraw(func() {
						cc.inModal = false
						cc.app.SetRoot(cc.mainFlex, true)
						cc.app.SetFocus(cc.servicesView)
					})
					cc.AddActivity("error", "Device authorization expired")
					return
				}
			case <-pollTicker.C:
				// Poll for token
				if cc.deviceAuth.PollToken == nil {
					continue
				}

				authkey, err := cc.deviceAuth.PollToken(config.DeviceCode, config.Interval)
				if err != nil {
					// Check if it's a pending error (keep polling)
					if strings.Contains(err.Error(), "pending") {
						continue
					}
					// Real error
					cc.app.QueueUpdateDraw(func() {
						updateContent("error:" + err.Error())
					})
					time.Sleep(2 * time.Second)
					cc.app.QueueUpdateDraw(func() {
						cc.inModal = false
						cc.app.SetRoot(cc.mainFlex, true)
						cc.app.SetFocus(cc.servicesView)
					})
					cc.AddActivity("error", fmt.Sprintf("Authorization failed: %v", err))
					return
				}

				// Success! Connect with the authkey
				cc.app.QueueUpdateDraw(func() {
					updateContent("success")
				})

				cc.AddActivity("success", "Authorization successful!")

				// Connect to network
				if cc.deviceAuth.Connect != nil {
					cc.AddActivity("info", "Connecting to network...")
					if err := cc.deviceAuth.Connect(authkey); err != nil {
						cc.AddActivity("error", fmt.Sprintf("Failed to connect: %v", err))
					} else {
						cc.AddActivity("success", "Connected to AceTeam Network")
						cc.refresh()
					}
				}

				time.Sleep(time.Second)
				cc.app.QueueUpdateDraw(func() {
					cc.inModal = false
					cc.app.SetRoot(cc.mainFlex, true)
					cc.app.SetFocus(cc.servicesView)
				})
				return
			}
		}
	}()
}

// showNodeDetailModal shows detailed node information
func (cc *ControlCenter) showNodeDetailModal() {
	var sb strings.Builder

	sb.WriteString("[yellow::b]Node Information[-:-:-]\n\n")

	nodeName := cc.data.NodeName
	if nodeName == "" {
		nodeName = "unknown"
	}

	sb.WriteString(fmt.Sprintf("[yellow]Name:[-]    %s\n", nodeName))
	if cc.data.OrgID != "" {
		sb.WriteString(fmt.Sprintf("[yellow]Org:[-]     %s\n", cc.data.OrgID))
	}
	if cc.data.UserEmail != "" {
		sb.WriteString(fmt.Sprintf("[yellow]User:[-]    %s\n", cc.data.UserEmail))
		if cc.data.UserName != "" && cc.data.UserName != cc.data.UserEmail {
			sb.WriteString(fmt.Sprintf("[yellow]Name:[-]    %s\n", cc.data.UserName))
		}
	}
	sb.WriteString(fmt.Sprintf("[yellow]Version:[-] %s\n", cc.data.Version))

	// Network status and IPs
	sb.WriteString("\n[yellow::b]Network[-:-:-]\n")

	if cc.data.DualConnection {
		// Both connections active
		sb.WriteString("[green]● Dual Connection Active[-]\n\n")

		sb.WriteString("[cyan]Citadel (embedded tsnet):[-]\n")
		sb.WriteString(fmt.Sprintf("  IP: [white]%s[-]\n", cc.data.NodeIP))
		sb.WriteString("  [gray]• App-specific networking (only Citadel)[-]\n")
		sb.WriteString("  [gray]• No root required (userspace)[-]\n")
		sb.WriteString("  [gray]• Services exposed via this IP[-]\n\n")

		sb.WriteString("[blue]System Tailscale:[-]\n")
		if cc.data.SystemTailscaleName != "" {
			sb.WriteString(fmt.Sprintf("  Name: %s\n", cc.data.SystemTailscaleName))
		}
		sb.WriteString(fmt.Sprintf("  IP: [white]%s[-]\n", cc.data.SystemTailscaleIP))
		sb.WriteString("  [gray]• System-wide VPN (all apps)[-]\n")
		sb.WriteString("  [gray]• Managed via 'tailscale' CLI[-]\n\n")

		sb.WriteString("[gray]Both can coexist and reach each other on the mesh.[-]\n")

	} else if cc.data.Connected {
		sb.WriteString("[green]● Connected via Citadel[-]\n")
		sb.WriteString(fmt.Sprintf("  IP: [white]%s[-]\n", cc.data.NodeIP))
		sb.WriteString("  [gray]Embedded tsnet - app-specific, no root needed[-]\n")

		if cc.data.SystemTailscaleRunning {
			sb.WriteString(fmt.Sprintf("\n[gray]System Tailscale also running: %s (%s)[-]\n",
				cc.data.SystemTailscaleName, cc.data.SystemTailscaleIP))
		}

	} else if cc.data.SystemTailscaleRunning {
		sb.WriteString("[blue]● Connected via System Tailscale[-]\n")
		if cc.data.SystemTailscaleName != "" {
			sb.WriteString(fmt.Sprintf("  Name: %s\n", cc.data.SystemTailscaleName))
		}
		sb.WriteString(fmt.Sprintf("  IP: [white]%s[-]\n", cc.data.SystemTailscaleIP))
		sb.WriteString("\n[gray]Press 0 to also connect via Citadel[-]\n")

	} else {
		sb.WriteString("[gray]○ Not connected[-]\n")
		sb.WriteString("[gray]Press 0 to connect to network[-]\n")
	}

	sb.WriteString("\n[gray]Press any key to close[-]")

	cc.showInfoModal("Node Details", sb.String())
}

// showSystemDetailModal shows detailed system information
func (cc *ControlCenter) showSystemDetailModal() {
	var sb strings.Builder

	sb.WriteString("[yellow::b]System Information[-:-:-]\n\n")

	// CPU
	sb.WriteString("[yellow]CPU[-]\n")
	sb.WriteString(fmt.Sprintf("  Usage: %.1f%%\n", cc.data.CPUPercent))

	// Memory
	sb.WriteString("\n[yellow]Memory[-]\n")
	sb.WriteString(fmt.Sprintf("  Usage: %.1f%%\n", cc.data.MemoryPercent))
	sb.WriteString(fmt.Sprintf("  Used:  %s / %s\n", cc.data.MemoryUsed, cc.data.MemoryTotal))

	// Disk
	sb.WriteString("\n[yellow]Disk[-]\n")
	sb.WriteString(fmt.Sprintf("  Usage: %.1f%%\n", cc.data.DiskPercent))
	sb.WriteString(fmt.Sprintf("  Used:  %s / %s\n", cc.data.DiskUsed, cc.data.DiskTotal))

	// GPU
	if cc.data.GPUName != "" {
		sb.WriteString("\n[yellow]GPU[-]\n")
		sb.WriteString(fmt.Sprintf("  Model:       %s\n", cc.data.GPUName))
		sb.WriteString(fmt.Sprintf("  Utilization: %.1f%%\n", cc.data.GPUUtilization))
		sb.WriteString(fmt.Sprintf("  Memory:      %s\n", cc.data.GPUMemory))
		sb.WriteString(fmt.Sprintf("  Temperature: %s\n", cc.data.GPUTemp))
	} else {
		sb.WriteString("\n[gray]No GPU detected[-]\n")
	}

	// OS info
	sb.WriteString(fmt.Sprintf("\n[yellow]Platform:[-] %s/%s\n", runtime.GOOS, runtime.GOARCH))

	sb.WriteString("\n[gray]Press any key to close[-]")

	cc.showInfoModal("System Details", sb.String())
}

// showJobsDetailModal shows detailed jobs/worker information with controls
func (cc *ControlCenter) showJobsDetailModal() {
	cc.inModal = true

	// Create a fullscreen view
	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)

	// Update function to rebuild content
	updateContent := func() {
		var sb strings.Builder

		sb.WriteString("[yellow::b]Worker & Queue Information[-:-:-]\n\n")

		// Worker status
		sb.WriteString("[yellow]Worker Status[-]\n")
		if cc.data.WorkerRunning {
			sb.WriteString("  [green]● Active[-]\n")
		} else {
			sb.WriteString("  [gray]○ Stopped[-]\n")
		}

		// Queue subscriptions
		sb.WriteString("\n[yellow]Queue Subscriptions[-]\n")
		if len(cc.data.Queues) > 0 {
			for _, q := range cc.data.Queues {
				statusIcon := "[gray]○[-]"
				if q.Connected {
					statusIcon = "[green]●[-]"
				}
				sb.WriteString(fmt.Sprintf("  %s %s [gray](%s)[-]", statusIcon, q.Name, q.Type))
				if q.PendingCount > 0 {
					sb.WriteString(fmt.Sprintf(" [yellow]%d pending[-]", q.PendingCount))
				}
				sb.WriteString("\n")
			}
		} else if cc.data.WorkerQueue != "" {
			sb.WriteString(fmt.Sprintf("  [green]●[-] %s\n", cc.data.WorkerQueue))
		} else if cc.data.WorkerRunning {
			sb.WriteString("  [green]●[-] default queue\n")
		} else {
			sb.WriteString("  [gray]not subscribed[-]\n")
		}

		// Job stats
		sb.WriteString("\n[yellow]Job Statistics[-]\n")
		sb.WriteString(fmt.Sprintf("  Pending:    %d\n", cc.data.Jobs.Pending))
		sb.WriteString(fmt.Sprintf("  Processing: %d\n", cc.data.Jobs.Processing))
		sb.WriteString(fmt.Sprintf("  Completed:  %d\n", cc.data.Jobs.Completed))
		if cc.data.Jobs.Failed > 0 {
			sb.WriteString(fmt.Sprintf("  Failed:     [red]%d[-]\n", cc.data.Jobs.Failed))
		}

		// Recent jobs - show all with full details
		recentJobs := cc.GetRecentJobs()
		if len(recentJobs) > 0 {
			sb.WriteString("\n[yellow]Recent Jobs (last 10)[-]\n")
			sb.WriteString("  [gray]TYPE              STATUS    DURATION   TIME[-]\n")
			for _, job := range recentJobs {
				statusIcon := "[green]✓ success[-]"
				if job.Status == "failed" {
					statusIcon = "[red]✗ failed [-]"
				} else if job.Status == "processing" {
					statusIcon = "[cyan]● running[-]"
				}

				timeStr := job.StartedAt.Format("15:04:05")
				durationStr := "-"
				if job.Duration > 0 {
					if job.Duration < time.Second {
						durationStr = fmt.Sprintf("%dms", job.Duration.Milliseconds())
					} else {
						durationStr = fmt.Sprintf("%.2fs", job.Duration.Seconds())
					}
				}

				jobType := job.Type
				if len(jobType) > 16 {
					jobType = jobType[:13] + "..."
				}

				sb.WriteString(fmt.Sprintf("  %-16s  %s  %8s   %s\n",
					jobType, statusIcon, durationStr, timeStr))

				if job.Error != "" {
					errMsg := job.Error
					if len(errMsg) > 50 {
						errMsg = errMsg[:47] + "..."
					}
					sb.WriteString(fmt.Sprintf("    [red]Error: %s[-]\n", errMsg))
				}
			}
		} else {
			sb.WriteString("\n[gray]No jobs processed yet[-]\n")
		}

		// Controls help
		sb.WriteString("\n[gray]─────────────────────────────────────────[-]\n")
		if cc.data.WorkerRunning {
			sb.WriteString("[yellow]s[-] Stop Worker  │  [yellow]Esc[-] Close")
		} else {
			sb.WriteString("[yellow]s[-] Start Worker  │  [yellow]Esc[-] Close")
		}

		textView.SetText(sb.String())
	}

	updateContent()
	textView.SetBorder(true).SetTitle(" Jobs (Full Screen) ")

	textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.updatePaneFocus()
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 's', 'S':
				if cc.data.WorkerRunning {
					cc.stopWorker()
				} else {
					cc.startWorker()
				}
				// Refresh view after a short delay
				go func() {
					time.Sleep(500 * time.Millisecond)
					cc.app.QueueUpdateDraw(func() {
						updateContent()
					})
				}()
				return nil
			}
		}
		return event
	})

	cc.app.SetRoot(textView, true)
	cc.app.SetFocus(textView)
}

// startWorker starts the worker via callbacks
func (cc *ControlCenter) startWorker() {
	if cc.worker.Start == nil {
		cc.AddActivity("error", "Worker management not available")
		return
	}

	if cc.worker.IsRunning != nil && cc.worker.IsRunning() {
		cc.AddActivity("info", "Worker is already running")
		return
	}

	go func() {
		cc.AddActivity("info", "Starting worker...")

		// Pass activity callback so worker can log to the TUI
		err := cc.worker.Start(cc.AddActivity)
		if err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to start worker: %v", err))
			return
		}

		cc.data.WorkerRunning = true
		cc.AddActivity("success", "Worker started")

		// Update the jobs panel
		cc.app.QueueUpdateDraw(func() {
			cc.updateJobsPanel()
		})
	}()
}

// stopWorker stops the worker via callbacks
func (cc *ControlCenter) stopWorker() {
	if cc.worker.Stop == nil {
		cc.AddActivity("error", "Worker management not available")
		return
	}

	if cc.worker.IsRunning != nil && !cc.worker.IsRunning() {
		cc.AddActivity("info", "Worker is not running")
		return
	}

	go func() {
		cc.AddActivity("info", "Stopping worker...")

		err := cc.worker.Stop()
		if err != nil {
			cc.AddActivity("error", fmt.Sprintf("Failed to stop worker: %v", err))
			return
		}

		cc.data.WorkerRunning = false
		cc.AddActivity("success", "Worker stopped")

		// Update the jobs panel
		cc.app.QueueUpdateDraw(func() {
			cc.updateJobsPanel()
		})
	}()
}

// showPeerDetailModal shows a modal with peer details and ping option
func (cc *ControlCenter) showPeerDetailModal() {
	if !cc.data.Connected || len(cc.data.Peers) == 0 {
		cc.AddActivity("info", "No peers available")
		return
	}

	row, _ := cc.peersView.GetSelection()
	if row < 1 || row > len(cc.data.Peers) {
		return
	}

	cc.inModal = true

	// Create a table for peer list with ping capability
	table := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	table.SetBorder(true).SetTitle(" Network Peers (p=ping, Enter=ping, Esc=close) ")

	// Header
	headers := []string{" ", "HOSTNAME", "IP", "STATUS", "LATENCY"}
	for i, h := range headers {
		cell := tview.NewTableCell("[yellow::b]" + h + "[-:-:-]").SetSelectable(false)
		table.SetCell(0, i, cell)
	}

	// Populate peers
	for i, p := range cc.data.Peers {
		tableRow := i + 1
		icon := "[gray]○[-]"
		statusText := "[gray]offline[-]"
		if p.Online {
			icon = "[green]●[-]"
			statusText = "[green]online[-]"
		}

		table.SetCell(tableRow, 0, tview.NewTableCell(icon).SetSelectable(true))
		table.SetCell(tableRow, 1, tview.NewTableCell(p.Hostname).SetSelectable(true))
		table.SetCell(tableRow, 2, tview.NewTableCell("[gray]"+p.IP+"[-]").SetSelectable(true))
		table.SetCell(tableRow, 3, tview.NewTableCell(statusText).SetSelectable(true))

		latency := p.Latency
		if latency == "" {
			latency = "-"
		}
		table.SetCell(tableRow, 4, tview.NewTableCell("[gray]"+latency+"[-]").SetSelectable(true))
	}

	// Select the peer we came from
	table.Select(row, 0)

	// Ping function
	pingSelected := func() {
		selectedRow, _ := table.GetSelection()
		if selectedRow < 1 || selectedRow > len(cc.data.Peers) {
			return
		}
		selectedPeer := cc.data.Peers[selectedRow-1]
		if selectedPeer.IP == "" {
			cc.AddActivity("warning", fmt.Sprintf("No IP for %s", selectedPeer.Hostname))
			return
		}

		// Update table to show pinging
		table.SetCell(selectedRow, 4, tview.NewTableCell("[yellow]pinging...[-]").SetSelectable(true))

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			latency, connType, relay, err := network.PingPeer(ctx, selectedPeer.IP)

			cc.app.QueueUpdateDraw(func() {
				if err != nil {
					table.SetCell(selectedRow, 4, tview.NewTableCell("[red]unreachable[-]").SetSelectable(true))
					cc.AddActivity("warning", fmt.Sprintf("%s: unreachable", selectedPeer.Hostname))
				} else {
					connInfo := connType
					if relay != "" {
						connInfo = fmt.Sprintf("relay:%s", relay)
					}
					latencyStr := fmt.Sprintf("%.1fms (%s)", latency, connInfo)
					table.SetCell(selectedRow, 4, tview.NewTableCell("[green]"+latencyStr+"[-]").SetSelectable(true))
					cc.AddActivity("success", fmt.Sprintf("%s: %.1fms (%s)", selectedPeer.Hostname, latency, connInfo))
				}
			})
		}()
	}

	table.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			cc.inModal = false
			cc.app.SetRoot(cc.mainFlex, true)
			cc.updatePaneFocus()
			return nil
		case tcell.KeyEnter:
			pingSelected()
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'p' || event.Rune() == 'P' {
				pingSelected()
				return nil
			}
		}
		return event
	})

	cc.app.SetRoot(table, true)
	cc.app.SetFocus(table)
}

// DetectSystemTailscale checks if system tailscale is running and on the same network
func DetectSystemTailscale(nexusURL string) (running bool, ip, name string, sameNetwork bool) {
	// Try to get tailscale status
	cmd := exec.Command("tailscale", "status", "--json")
	output, err := cmd.Output()
	if err != nil {
		return false, "", "", false
	}

	// Parse JSON properly
	var status struct {
		BackendState string `json:"BackendState"`
		Self         *struct {
			HostName     string   `json:"HostName"`
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		} `json:"Self"`
		ControlURL string `json:"ControlURL"`
	}

	if err := json.Unmarshal(output, &status); err != nil {
		// Fallback to string matching for older tailscale versions
		outStr := string(output)
		if !strings.Contains(outStr, `"BackendState":"Running"`) {
			return false, "", "", false
		}
		running = true
		// Try to extract IP from string
		if idx := strings.Index(outStr, `"TailscaleIPs":["`); idx != -1 {
			start := idx + len(`"TailscaleIPs":["`)
			end := strings.Index(outStr[start:], `"`)
			if end > 0 {
				ip = outStr[start : start+end]
			}
		}
		return running, ip, name, false
	}

	// Check if running
	if status.BackendState != "Running" {
		return false, "", "", false
	}
	running = true

	// Extract self info
	if status.Self != nil {
		name = status.Self.HostName
		if len(status.Self.TailscaleIPs) > 0 {
			// Prefer IPv4
			for _, addr := range status.Self.TailscaleIPs {
				if !strings.Contains(addr, ":") { // IPv4
					ip = addr
					break
				}
			}
			if ip == "" {
				ip = status.Self.TailscaleIPs[0]
			}
		}
	}

	// Check if same network by comparing control URL
	if status.ControlURL != "" {
		// Compare control URLs (handle with/without trailing slash, http vs https)
		controlURL := strings.TrimSuffix(status.ControlURL, "/")
		nexusClean := strings.TrimSuffix(nexusURL, "/")

		if controlURL == nexusClean ||
			strings.TrimPrefix(controlURL, "https://") == strings.TrimPrefix(nexusClean, "https://") ||
			strings.Contains(controlURL, strings.TrimPrefix(nexusClean, "https://")) {
			sameNetwork = true
		}
	}

	return running, ip, name, sameNetwork
}
