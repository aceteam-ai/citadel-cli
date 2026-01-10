// internal/nexus/network_helpers.go
package nexus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/aceboss/citadel-cli/internal/ui"
)

// getTailscaleCLI returns the path to the tailscale CLI executable.
// On Windows, we use the full path because the PATH might not include
// the Tailscale installation directory in all contexts.
func getTailscaleCLI() string {
	if runtime.GOOS == "windows" {
		fullPath := `C:\Program Files\Tailscale\tailscale.exe`
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath
		}
	}
	return "tailscale"
}

// NetworkChoice represents the user's selected method for network connection.
type NetworkChoice string

const (
	// NetChoiceDevice indicates the user will use device authorization flow.
	NetChoiceDevice NetworkChoice = "device"
	// NetChoiceAuthkey indicates the user will provide a pre-generated key.
	NetChoiceAuthkey NetworkChoice = "authkey"
	// NetChoiceBrowser indicates the user will log in via a web browser.
	NetChoiceBrowser NetworkChoice = "browser"
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
			"Log in with a browser (Legacy Headscale OAuth)",
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
	case strings.Contains(selection, "browser"):
		return NetChoiceBrowser, "", nil
	default:
		return NetChoiceSkip, "", nil
	}
}
