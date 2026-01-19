// internal/nexus/network_helpers.go
package nexus

import (
	"context"
	"fmt"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
)

// NetworkChoice represents the user's selected method for network connection.
type NetworkChoice string

const (
	// NetChoiceDevice indicates the user will use device authorization flow.
	NetChoiceDevice NetworkChoice = "device"
	// NetChoiceAuthkey indicates the user will provide a pre-generated key.
	NetChoiceAuthkey NetworkChoice = "authkey"
	// NetChoiceSkip indicates the user has chosen to skip network connection.
	NetChoiceSkip NetworkChoice = "skip"
	// NetChoiceVerified indicates the user is already online and authenticated.
	NetChoiceVerified NetworkChoice = "verified"
)

// IsNetworkConnected checks if currently connected to the AceTeam Network.
func IsNetworkConnected() bool {
	return network.IsGlobalConnected()
}

// NetworkLogout logs out of the current network connection.
func NetworkLogout() error {
	fmt.Println("   - Logging out of current network connection...")
	if err := network.Logout(); err != nil {
		return fmt.Errorf("failed to logout: %w", err)
	}
	fmt.Println("   - ✅ Successfully logged out.")
	return nil
}

// GetNetworkChoice checks the current network status and, if offline, prompts the
// user to select a connection method. It accepts the authkey from a flag as a parameter.
func GetNetworkChoice(authkey string) (choice NetworkChoice, key string, err error) {
	if authkey != "" {
		return NetChoiceAuthkey, authkey, nil
	}

	// Check if already connected
	if network.IsGlobalConnected() {
		return NetChoiceVerified, "", nil
	}

	// If state exists, try to reconnect
	if network.HasState() {
		fmt.Print("Connecting... ")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		connected, reconnectErr := network.VerifyOrReconnect(ctx)
		if connected {
			fmt.Println("done")
			return NetChoiceVerified, "", nil
		}
		if reconnectErr != nil {
			fmt.Printf("failed: %v\n", reconnectErr)
		} else {
			fmt.Println("failed")
		}
		// Fall through to prompt for new auth method
	}

	// Default to device authorization flow
	// Use --authkey flag for automation/CI
	return NetChoiceDevice, "", nil
}
