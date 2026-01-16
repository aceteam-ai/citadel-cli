// internal/nexus/network_helpers.go
package nexus

import (
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
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
		fmt.Println("✅ Authkey provided via flag.")
		return NetChoiceAuthkey, authkey, nil
	}

	fmt.Println("--- Checking network status...")

	// Check if connected using the network package
	if network.IsGlobalConnected() || network.HasState() {
		fmt.Println("   - ✅ Already connected to the AceTeam Network.")
		return NetChoiceVerified, "", nil
	}

	fmt.Println("   - ⚠️  You are not connected to the AceTeam Network.")
	selection, err := ui.AskSelect(
		"How would you like to connect this node?",
		[]string{
			"Device authorization (Recommended)",
			"Use a pre-generated authkey (For automation)",
			"Skip network connection for now",
		},
	)
	if err != nil {
		return "", "", err
	}

	switch {
	case strings.Contains(selection, "Device authorization"):
		return NetChoiceDevice, "", nil
	case strings.Contains(selection, "authkey"):
		keyInput, err := ui.AskInput("Enter your AceTeam authkey:", "tskey-auth-...", "")
		if err != nil {
			return "", "", err
		}
		return NetChoiceAuthkey, strings.TrimSpace(keyInput), nil
	default:
		return NetChoiceSkip, "", nil
	}
}
