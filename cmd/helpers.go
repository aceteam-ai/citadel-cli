// cmd/helpers.go
package cmd

import (
	"fmt"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/aceboss/citadel-cli/internal/platform"
	"github.com/aceboss/citadel-cli/internal/ui"
)

// runDeviceAuthFlow executes the OAuth 2.0 device authorization flow
// and returns the token response upon successful authorization
func runDeviceAuthFlow(authServiceURL string) (*nexus.TokenResponse, error) {
	fmt.Println("--- Starting device authorization flow ---")

	client := nexus.NewDeviceAuthClient(authServiceURL)

	// Start the flow and get device code
	resp, err := client.StartFlow()
	if err != nil {
		return nil, fmt.Errorf("failed to start device authorization: %w", err)
	}

	// Ask user if they want to open the browser
	fmt.Println("\nTo complete authorization, you need to visit the following URL:")
	fmt.Printf("  %s\n\n", resp.VerificationURI)

	openBrowser, err := ui.AskSelect(
		"Would you like to open this URL in your default browser?",
		[]string{"Yes (recommended)", "No, I'll open it manually"},
	)
	if err != nil {
		// If prompt fails, continue without opening browser
		fmt.Println("Continuing without opening browser...")
	} else if openBrowser == "Yes (recommended)" {
		// Open browser with complete URL (includes code pre-filled)
		urlToOpen := resp.VerificationURIComplete
		if urlToOpen == "" {
			// Fallback if complete URI not provided
			urlToOpen = resp.VerificationURI
		}

		if err := platform.OpenURL(urlToOpen); err != nil {
			fmt.Printf("⚠️  Could not open browser automatically: %v\n", err)
			fmt.Println("Please open the URL manually.")
		} else {
			fmt.Println("✓ Browser opened successfully")
		}
	}
	fmt.Println()

	// Create UI program
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
		fmt.Println("✅ Authorization successful! Received authentication key.")
		return token, nil
	case err := <-errChan:
		return nil, fmt.Errorf("device authorization failed: %w", err)
	default:
		return nil, fmt.Errorf("device authorization was canceled")
	}
}
