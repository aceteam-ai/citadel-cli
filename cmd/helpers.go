// cmd/helpers.go
package cmd

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
)

// runDeviceAuthFlow executes the OAuth 2.0 device authorization flow
// and returns the token response upon successful authorization
func runDeviceAuthFlow(authServiceURL string) (*nexus.TokenResponse, error) {
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
		return token, nil
	case err := <-errChan:
		return nil, fmt.Errorf("device authorization failed: %w", err)
	default:
		return nil, fmt.Errorf("device authorization was canceled")
	}
}
