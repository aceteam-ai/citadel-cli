// internal/nexus/network_helpers.go
package nexus

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
)

// getTailscaleCLI returns the path to the tailscale CLI executable.
// Delegates to the centralized platform.GetTailscaleCLI() which handles
// PATH lookup and platform-specific fallback locations for Windows, macOS, and Linux.
func getTailscaleCLI() string {
	return platform.GetTailscaleCLI()
}

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

type tailscaleStatus struct {
	Self struct {
		Online bool `json:"Online"`
	} `json:"Self"`
}

// IsTailscaleConnected checks if Tailscale is currently connected to a network.
func IsTailscaleConnected() bool {
	tailscaleCLI := getTailscaleCLI()
	statusCmd := exec.Command(tailscaleCLI, "status", "--json")
	output, err := statusCmd.Output()
	if err != nil {
		return false
	}

	var status tailscaleStatus
	if json.Unmarshal(output, &status) == nil && status.Self.Online {
		return true
	}
	return false
}

// TailscaleLogout logs out of the current Tailscale network.
func TailscaleLogout() error {
	tailscaleCLI := getTailscaleCLI()
	fmt.Println("   - Logging out of current Tailscale connection...")
	logoutCmd := exec.Command(tailscaleCLI, "logout")
	if output, err := logoutCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to logout from Tailscale: %s", string(output))
	}
	fmt.Println("   - ✅ Successfully logged out of Tailscale.")
	return nil
}

// GetNetworkChoice checks the current network status and, if offline, prompts the
// user to select a connection method. It accepts the authkey from a flag as a parameter.
func GetNetworkChoice(authkey string) (choice NetworkChoice, key string, err error) {
	if authkey != "" {
		fmt.Println("✅ Authkey provided via flag.")
		return NetChoiceAuthkey, authkey, nil
	}

	tailscaleCLI := getTailscaleCLI()
	fmt.Println("--- Checking network status...")
	statusCmd := exec.Command(tailscaleCLI, "status", "--json")
	output, _ := statusCmd.Output()

	var status tailscaleStatus
	if json.Unmarshal(output, &status) == nil && status.Self.Online {
		fmt.Println("   - ✅ Already connected to the Nexus network.")
		return NetChoiceVerified, "", nil
	}

	fmt.Println("   - ⚠️  You are not connected to the Nexus network.")
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
		keyInput, err := ui.AskInput("Enter your Nexus authkey:", "tskey-auth-...", "")
		if err != nil {
			return "", "", err
		}
		return NetChoiceAuthkey, strings.TrimSpace(keyInput), nil
	default:
		return NetChoiceSkip, "", nil
	}
}
