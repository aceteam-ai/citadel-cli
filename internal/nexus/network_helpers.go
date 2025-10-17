// internal/nexus/network_helpers.go
package nexus

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/aceboss/citadel-cli/internal/ui"
)

// NetworkChoice represents the user's selected method for network connection.
type NetworkChoice string

const (
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

// GetNetworkChoice checks the current network status and, if offline, prompts the
// user to select a connection method. It accepts the authkey from a flag as a parameter.
func GetNetworkChoice(authkey string) (choice NetworkChoice, key string, err error) {
	if authkey != "" {
		fmt.Println("✅ Authkey provided via flag.")
		return NetChoiceAuthkey, authkey, nil
	}

	fmt.Println("--- Checking network status...")
	statusCmd := exec.Command("tailscale", "status", "--json")
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
			"Use a pre-generated authkey (Recommended for servers)",
			"Log in with a browser",
			"Skip network connection for now",
		},
	)
	if err != nil {
		return "", "", err
	}

	switch {
	case strings.Contains(selection, "authkey"):
		keyInput, err := ui.AskInput("Enter your Nexus authkey:", "nexus-auth-...", "")
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
