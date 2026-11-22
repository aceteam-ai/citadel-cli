// cmd/enroll.go
package cmd

import (
	"context"
	"errors"
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
// auth protocol. The sequence itself is enrollDevice (cmd/enroll_core.go); this
// command supplies the terminal pieces (QR UI, name prompt, printed progress).
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

		// Refuse an explicit --nexus that differs from the control plane this
		// node is already enrolled against (citadel-cli#1110).
		if err := refuseNexusFlagMismatch(cmd); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		opts := enrollOptions{
			AuthServiceURL:   authServiceURL,
			ConfigDir:        nodeConfigDirFn(),
			NodeName:         enrollNodeName,
			ForceNew:         enrollNewDevice,
			NexusFlagChanged: flagChanged(cmd, "nexus"),
			OnStage: func(p enrollProgress) {
				if p.Stage == enrollStageConnect {
					fmt.Println("\n--- 🌐 Joining your org's Fabric ---")
					fmt.Printf("Connecting as '%s'...\n", p.NodeName)
				}
			},
			OnWarn: func(msg string) { fmt.Fprintln(os.Stderr, msg) },
		}
		deps, err := terminalEnrollDeps(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
		result, err := enrollDevice(ctx, opts, deps)
		if err != nil {
			if kind, _ := enrollFailureKind(err); kind == enrollAlreadyConnected {
				// Tell the user instead of re-enrolling silently.
				nodeName, _ := getNodeName()
				ip, _ := network.GetGlobalIPv4()
				fmt.Printf("✅ This node is already connected to the mesh as '%s'.\n", nodeName)
				printDeviceLinkDiagnosticContext(cmd.Context(), os.Stderr, getDeviceConfigFromFile())
				if ip != "" {
					fmt.Printf("   IP: %s\n", ip)
				}
				fmt.Println("\n   Run 'citadel status' to view Fabric details.")
				fmt.Println("   To re-enroll into a different org: citadel logout && citadel enroll")
				return
			}
			reportEnrollFailure(err)
			os.Exit(1)
		}

		ip, _ := network.GetGlobalIPv4()
		printNetworkSuccessInfo(result.NodeName, ip)
		if result.OrgName != "" {
			fmt.Printf("   Org: %s\n", result.OrgName)
		}
		fmt.Println("\n✅ This node is now enrolled in your Fabric.")
		fmt.Println("   Run 'citadel run' to start serving, or 'citadel status' to view details.")
	},
}

// terminalEnrollDeps are the enrollment side effects for `citadel enroll` on a
// terminal: the QR/bubbletea device flow, the interactive friendlier-name
// offer, and the printed reclaim and IP lines, exactly as before the core was
// extracted.
func terminalEnrollDeps(opts enrollOptions) (enrollDeps, error) {
	deps, err := defaultEnrollDeps(opts)
	if err != nil {
		return deps, err
	}
	deps.authorize = func(context.Context) (*DeviceAuthResult, error) {
		fmt.Println("Scan the QR code below with the AceTeam app to add this node to your Fabric.")

		// Run the device-authorization flow. This renders the enrollment QR
		// (see internal/ui devicecode/qrcode) and blocks until the scan is
		// approved or the code expires.
		return runDeviceAuthFlow(opts.AuthServiceURL, opts.ForceNew)
	}
	// Resolve the node name (flag > saved > hostname, with the interactive
	// offer for a generated name).
	deps.nodeName = func(explicit string) (string, error) {
		if explicit != "" {
			return explicit, nil
		}
		return getNodeName()
	}
	deps.reclaim = reclaimStaleNodeByHostname
	deps.connect = func(r enrollConnectRequest) error {
		ip, err := registerOnNetworkWith(r.NodeName, r.AuthKey, r.ForceLogin, r.ControlURL, r.AuthURL)
		if err == nil && ip != "" {
			fmt.Printf("   IP: %s\n", ip)
		}
		return err
	}
	return deps, nil
}

// reportEnrollFailure prints an enrollDevice failure the way `citadel enroll`
// always has.
func reportEnrollFailure(err error) {
	var ee *enrollError
	if !errors.As(err, &ee) {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		return
	}
	switch {
	case ee.Stage == enrollStageCheck && ee.Kind == enrollUnreachable:
		// Preflight: the API was not reachable before interactive auth began.
		fmt.Fprintf(os.Stderr, "❌ Cannot reach AceTeam API: %v\n", err)
		fmt.Fprintln(os.Stderr, "\nCheck your internet connection and try again.")
	case ee.Stage == enrollStageAuthorize:
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		if nexus.IsNetworkError(err) {
			fmt.Fprintln(os.Stderr, "\nThe API became unreachable during enrollment.")
			fmt.Fprintln(os.Stderr, "Check your network connection and try again.")
		} else {
			fmt.Fprintf(os.Stderr, "\nAlternative: generate an authkey at %s/fabric\n", authServiceURL)
			fmt.Fprintln(os.Stderr, "Then run: citadel login --authkey <your-key>")
		}
	case ee.Kind == enrollAlreadyEnrolled:
		fmt.Fprintln(os.Stderr, "❌ This machine is already enrolled in the Fabric.")
		fmt.Fprintln(os.Stderr, "   To re-enroll: citadel logout && citadel enroll")
		fmt.Fprintln(os.Stderr, "   To replace this registration with a new one: citadel enroll --new-device")
	case ee.Kind == enrollNodeNameFailed:
		fmt.Fprintf(os.Stderr, "❌ Error getting node name: %v\n", err)
	case ee.Kind == enrollConnectFailed:
		fmt.Fprintf(os.Stderr, "❌ Failed to join the Fabric: %v\n", err)
	case ee.Kind == enrollLinkFailed:
		fmt.Fprintf(os.Stderr, "❌ Enrollment incomplete: %v\n", err)
	default:
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
	}
}

func init() {
	rootCmd.AddCommand(enrollCmd)
	enrollCmd.Flags().StringVar(&enrollNodeName, "node-name", "", "Set the node name (defaults to hostname)")
	enrollCmd.Flags().BoolVar(&enrollNewDevice, "new-device", false, "Force fresh registration, ignoring any existing machine mapping")
}
