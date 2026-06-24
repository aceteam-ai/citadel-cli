// cmd/helpers.go
package cmd

import (
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/fatih/color"
)

// DeviceAuthResult contains the result of a device authorization flow.
type DeviceAuthResult struct {
	Token      *nexus.TokenResponse
	DeviceCode string // The device code used during auth (for config lookup)
}

// runDeviceAuthFlow executes the OAuth 2.0 device authorization flow
// and returns the token response and device code upon successful authorization.
// The device code is returned for use in status publishing to enable config lookup.
// If forceNew is true, the backend will ignore existing machine mappings and
// create a fresh device registration.
//
// When no TTY is available (e.g. SSH without -t, CI, pipes), the device code
// and URL are printed as plain text and the flow polls without bubbletea.
func runDeviceAuthFlow(authServiceURL string, forceNew bool) (*DeviceAuthResult, error) {
	client := nexus.NewDeviceAuthClient(authServiceURL)

	// Start the flow and get device code
	opts := &nexus.StartFlowOptions{ForceNew: forceNew}
	resp, err := client.StartFlow(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to start device authorization: %w", err)
	}

	// Non-TTY path: print plain text and poll without bubbletea
	if !tui.IsTTY() {
		completeURL := resp.VerificationURI + "?code=" + resp.UserCode
		fmt.Println()
		fmt.Println("Device authorization required.")
		fmt.Println("Scan this QR with the AceTeam app to add this node to your Fabric:")
		fmt.Println()
		fmt.Print(ui.RenderEnrollQR(resp.VerificationURI, resp.UserCode))
		fmt.Println()
		fmt.Printf("Or open this URL to sign in: %s\n", completeURL)
		fmt.Printf("Or enter code manually:      %s\n", resp.UserCode)
		fmt.Println()
		fmt.Println("Waiting for authorization...")

		token, err := client.PollForToken(resp.DeviceCode, resp.Interval)
		if err != nil {
			return nil, fmt.Errorf("device authorization failed: %w", err)
		}

		fmt.Println("Authorization successful!")
		return &DeviceAuthResult{
			Token:      token,
			DeviceCode: resp.DeviceCode,
		}, nil
	}

	// TTY path: interactive bubbletea UI
	model := ui.NewDeviceCodeModel(resp.UserCode, resp.VerificationURI, resp.ExpiresIn)
	program := ui.NewDeviceCodeProgram(model)

	// Start polling in background goroutine
	tokenChan := make(chan *nexus.TokenResponse, 1)
	errChan := make(chan error, 1)
	doneChan := make(chan struct{})

	go func() {
		defer close(doneChan)
		token, err := client.PollForToken(resp.DeviceCode, resp.Interval)
		if err != nil {
			errChan <- err
			ui.UpdateStatus(program, "error:"+err.Error())
			return
		}
		tokenChan <- token
		ui.UpdateStatus(program, "approved")
	}()

	// Run the UI (blocks until approved, error, or user quits)
	fmt.Println()
	if _, err := program.Run(); err != nil {
		return nil, fmt.Errorf("UI error: %w", err)
	}
	fmt.Println()

	// Wait for polling goroutine to complete (with timeout for safety)
	// The goroutine should complete quickly after UI exits since it sent the status
	select {
	case token := <-tokenChan:
		fmt.Println("✅ Authorization successful!")
		return &DeviceAuthResult{
			Token:      token,
			DeviceCode: resp.DeviceCode,
		}, nil
	case err := <-errChan:
		return nil, fmt.Errorf("device authorization failed: %w", err)
	case <-time.After(2 * time.Second):
		// If UI exited but no result yet, user likely canceled
		return nil, fmt.Errorf("device authorization was canceled")
	}
}

// printNetworkSuccessInfo displays helpful post-connection info explaining
// userspace networking limitations and available peer commands.
func printNetworkSuccessInfo(nodeName, ip string) {
	successColor := color.New(color.FgGreen, color.Bold)
	dimColor := color.New(color.Faint)

	fmt.Println()
	successColor.Println("✅ Successfully connected to the AceTeam Network!")
	fmt.Println()

	// Display node info
	if nodeName != "" {
		fmt.Printf("   Node:    %s\n", nodeName)
	}
	if ip != "" {
		fmt.Printf("   IP:      %s\n", ip)
	}
	fmt.Println()

	// Explain userspace networking limitation
	dimColor.Println("   This IP is for AceTeam network traffic only.")
	dimColor.Println("   System tools (ping, curl) cannot reach it directly.")
	fmt.Println()

	// Available commands
	fmt.Println("   Next steps:")
	fmt.Println("     citadel status    - View network status and peers")
	fmt.Println("     citadel ssh       - SSH to other nodes")
	fmt.Println("     citadel proxy     - Forward local ports to peers")
	fmt.Println()

	// Note: system-wide access is handled by the embedded tsnet library.
	// No separate system-level command is needed.
}
