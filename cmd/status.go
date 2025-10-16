// cmd/status.go
/*
Copyright © 2025 Jason Sun <jason@aceteam.ai>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// TailscaleStatus represents the relevant fields from `tailscale status --json`
type TailscaleStatus struct {
	Self struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Self"`
}

// statusCmd represents the status command
var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Shows the status of the Citadel node and its services",
	Long: `Provides a health check of the Citadel node. It checks:
1. Network Status: Verifies connection to the AceTeam Nexus via Tailscale.
2. Service Status: Checks the state of containers defined in citadel.yaml.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("--- Citadel Node Status ---")

		// 1. Check Network Status
		fmt.Println("\n🌐 Network Status:")
		tsCmd := exec.Command("tailscale", "status", "--json")
		output, err := tsCmd.Output()
		if err != nil {
			fmt.Println("  ❌ Could not get Tailscale status. Is the daemon running? Try 'sudo systemctl start tailscaled'")
			fmt.Fprintf(os.Stderr, "     Error: %v\n", err)
		} else {
			var status TailscaleStatus
			if err := json.Unmarshal(output, &status); err != nil {
				fmt.Println("  ❌ Could not parse Tailscale status JSON.")
			} else {
				if status.Self.Online {
					fmt.Println("  ✅ Connected to Nexus")
					fmt.Printf("     - Hostname: %s\n", strings.TrimSuffix(status.Self.DNSName, "."))
					fmt.Printf("     - IP Address: %s\n", status.Self.TailscaleIPs[0])
				} else {
					fmt.Println("  ⚠️  Not connected to Nexus. Node is offline.")
				}
			}
		}

		// 2. Check Service Status
		fmt.Println("\n🚀 Service Status:")
		manifest, err := readManifest("citadel.yaml")
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("  🤷 No citadel.yaml found, no manifest services to check.")
				return
			}
			fmt.Fprintf(os.Stderr, "  ❌ Error reading manifest: %v\n", err)
			return
		}

		if len(manifest.Services) == 0 {
			fmt.Println("  ℹ️  Manifest contains no services.")
			return
		}

		for _, service := range manifest.Services {
			fmt.Printf("  - Service: %s (%s)\n", service.Name, service.ComposeFile)
			if _, err := os.Stat(service.ComposeFile); os.IsNotExist(err) {
				fmt.Printf("    ❌ Status: Compose file not found.\n")
				continue
			}
			psCmd := exec.Command("docker", "compose", "-f", service.ComposeFile, "ps", "--format", "pretty")
			psOut, err := psCmd.CombinedOutput()
			if err != nil {
				fmt.Printf("    ❌ Could not get status: %v\n", err)
			} else {
				// Indent the output for better readability
				for _, line := range strings.Split(strings.TrimSpace(string(psOut)), "\n") {
					fmt.Printf("    %s\n", line)
				}
			}
		}
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
