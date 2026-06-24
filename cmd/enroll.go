// cmd/enroll.go
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/spf13/cobra"
)

var (
	enrollNodeName  string
	enrollNewDevice bool
)

// enrollCmd binds this node to an org's Fabric via a scannable QR code.
//
// It is a thin, discoverable entry point over the existing device-authorization
// flow (RFC 8628): it shows a QR encoding the verification URL, the operator
// scans it with the AceTeam iOS app (or a phone camera), approves, and the node
// joins the org's Headscale user (org_<id>) — authenticated and org-scoped.
//
// This is the QR-forward sibling of `citadel login` / `citadel init`; it reuses
// the same machinery (runDeviceAuthFlow + connectToNetwork), introducing no new
// auth protocol.
var enrollCmd = &cobra.Command{
	Use:   "enroll",
	Short: "Bind this node to your org's Fabric by scanning a QR code",
	Long: `Enroll connects this node to your AceTeam organization's Fabric.

It displays a QR code that you scan with the AceTeam app (or a phone camera).
Once you approve the device, the node joins your org's private mesh
(Headscale user org_<id>), authenticated and scoped to your organization, and
appears in the Fabric dashboard.

This is the easiest way to add a freshly installed or booted Citadel node:
just run 'citadel enroll' and scan.`,
	Example: `  # Scan-to-enroll this node into your org's Fabric
  citadel enroll

  # Enroll with a specific node name
  citadel enroll --node-name gpu-rig-01

  # Force a fresh device registration (ignore any prior machine mapping)
  citadel enroll --new-device`,
	Run: func(cmd *cobra.Command, args []string) {
		nexus.DebugFunc = Debug

		// Already connected? Tell the user instead of re-enrolling silently.
		if network.IsGlobalConnected() {
			nodeName, _ := getNodeName()
			ip, _ := network.GetGlobalIPv4()
			fmt.Printf("✅ This node is already enrolled as '%s'.\n", nodeName)
			if ip != "" {
				fmt.Printf("   IP: %s\n", ip)
			}
			fmt.Println("\n   Run 'citadel status' to view Fabric details.")
			fmt.Println("   To re-enroll into a different org: citadel logout && citadel enroll")
			return
		}

		// Preflight: verify the API is reachable before starting interactive auth.
		if err := nexus.CheckAPIReachable(authServiceURL); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Cannot reach AceTeam API: %v\n", err)
			fmt.Fprintln(os.Stderr, "\nCheck your internet connection and try again.")
			os.Exit(1)
		}

		fmt.Println("Scan the QR code below with the AceTeam app to add this node to your Fabric.")

		// Run the device-authorization flow. This renders the enrollment QR
		// (see internal/ui devicecode/qrcode) and blocks until the scan is
		// approved or the code expires.
		result, err := runDeviceAuthFlow(authServiceURL, enrollNewDevice)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			if nexus.IsNetworkError(err) {
				fmt.Fprintln(os.Stderr, "\nThe API became unreachable during enrollment.")
				fmt.Fprintln(os.Stderr, "Check your network connection and try again.")
			} else {
				fmt.Fprintf(os.Stderr, "\nAlternative: generate an authkey at %s/fabric\n", authServiceURL)
				fmt.Fprintln(os.Stderr, "Then run: citadel login --authkey <your-key>")
			}
			os.Exit(1)
		}

		// Persist device config (org id, API token, etc.) so the binding
		// survives across sessions.
		if result.Token.DeviceAPIToken != "" {
			if err := saveDeviceConfigToFile(result.Token); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: could not save device config: %v\n", err)
			}
		} else if result.Token.RedisURL != "" {
			if err := saveRedisURLToConfig(result.Token.RedisURL); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: could not save config: %v\n", err)
			}
		}

		// Resolve the node name (flag > saved > hostname).
		nodeName := enrollNodeName
		if nodeName == "" {
			nodeName, err = getNodeName()
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error getting node name: %v\n", err)
				os.Exit(1)
			}
		}

		// Reclaim any stale registration with the same hostname so the node
		// does not appear twice in the Fabric dashboard after re-installs.
		reclaimStaleNodeByHostname(result.Token.DeviceAPIToken, nodeName)

		fmt.Println("\n--- 🌐 Joining your org's Fabric ---")
		fmt.Printf("Connecting as '%s'...\n", nodeName)
		if err := connectToNetwork(nodeName, result.Token.Authkey); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to join the Fabric: %v\n", err)
			os.Exit(1)
		}

		ip, _ := network.GetGlobalIPv4()
		printNetworkSuccessInfo(nodeName, ip)
		if result.Token.OrgName != "" {
			fmt.Printf("   Org: %s\n", result.Token.OrgName)
		}
		fmt.Println("\n✅ This node is now enrolled in your Fabric.")
		fmt.Println("   Run 'citadel run' to start serving, or 'citadel status' to view details.")
	},
}

func init() {
	rootCmd.AddCommand(enrollCmd)
	enrollCmd.Flags().StringVar(&enrollNodeName, "node-name", "", "Set the node name (defaults to hostname)")
	enrollCmd.Flags().BoolVar(&enrollNewDevice, "new-device", false, "Force fresh registration, ignoring any existing machine mapping")
}
