// cmd/helpers.go
package cmd

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
)

// DeviceAuthResult contains the result of a device authorization flow.
type DeviceAuthResult struct {
	Token      *nexus.TokenResponse
	DeviceCode string // The device code used during auth (for config lookup)
}

// runDeviceAuthFlow executes the OAuth 2.0 device authorization flow
// and returns the token response and device code upon successful authorization.
// The device code is returned for use in status publishing to enable config lookup.
func runDeviceAuthFlow(authServiceURL string) (*DeviceAuthResult, error) {
	client := nexus.NewDeviceAuthClient(authServiceURL)

	// Start the flow and get device code
	resp, err := client.StartFlow()
	if err != nil {
		return nil, fmt.Errorf("failed to start device authorization: %w", err)
	}

	// Create UI program with device code display
	// The UI shows clickable links and keyboard shortcuts for browser/clipboard
	model := ui.NewDeviceCodeModel(resp.UserCode, resp.VerificationURI, resp.ExpiresIn)
	program := ui.NewDeviceCodeProgram(model)

	// Start polling in background goroutine
	tokenChan := make(chan *nexus.TokenResponse, 1)
	errChan := make(chan error, 1)

	go func() {
		token, err := client.PollForToken(resp.DeviceCode, resp.Interval)
		if err != nil {
			errChan <- err
			ui.UpdateStatus(program, "error:"+err.Error())
			return
		}
		tokenChan <- token
		ui.UpdateStatus(program, "approved")
	}()

	// Run the UI (blocks until approved or error)
	fmt.Println()
	if _, err := program.Run(); err != nil {
		return nil, fmt.Errorf("UI error: %w", err)
	}
	fmt.Println()

	// Check results
	select {
	case token := <-tokenChan:
		fmt.Println("✅ Authorization successful!")
		return &DeviceAuthResult{
			Token:      token,
			DeviceCode: resp.DeviceCode,
		}, nil
	case err := <-errChan:
		return nil, fmt.Errorf("device authorization failed: %w", err)
	default:
		return nil, fmt.Errorf("device authorization was canceled")
	}
}
