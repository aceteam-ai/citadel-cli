// cmd/login.go
package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/tui/whimsy"
	"github.com/spf13/cobra"
)

var (
	loginAuthkey  string
	loginNodeName string
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate this machine with the AceTeam Network",
	Long: `Connects this machine to your AceTeam network. If already connected, it does
nothing. Otherwise, it interactively prompts for an authentication method.

Use --authkey for non-interactive authentication (ideal for automation).`,
	Example: `  # Interactive login (prompts for auth method)
  citadel login

  # Non-interactive login with authkey (for automation)
  citadel login --authkey tskey-auth-xxx

  # Override the node name
  citadel login --authkey tskey-auth-xxx --node-name my-gpu-server`,
	Run: func(cmd *cobra.Command, args []string) {
		// Non-interactive mode when authkey is provided
		if loginAuthkey != "" {
			runNonInteractiveLogin()
			return
		}

		// Interactive mode
		runInteractiveLogin()
	},
}

// runNonInteractiveLogin handles login with --authkey flag (formerly 'citadel join')
func runNonInteractiveLogin() {
	// Check if already connected
	if network.IsGlobalConnected() {
		fmt.Println("Already connected to the AceTeam Network.")
		return
	}

	// Get node name: prefer flag, then saved hostname, then OS hostname
	nodeName := loginNodeName
	if nodeName == "" {
		if saved := getSavedHostname(); saved != "" {
			nodeName = saved
		}
	}
	if nodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not determine hostname: %v\n", err)
			os.Exit(1)
		}
		nodeName = hostname
	}

	// Persist hostname for reboot stability
	if err := saveHostnameToConfig(nodeName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save hostname: %v\n", err)
	}

	// Try to reclaim stale node with the same hostname
	if savedConfig := getDeviceConfigFromFile(); savedConfig != nil {
		reclaimStaleNodeByHostname(savedConfig.DeviceAPIToken, nodeName)
	}

	// Connect to the network with animated spinner
	spinner := whimsy.NewSimpleSpinner(whimsy.ConnectingMessages)
	spinner.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config := network.ServerConfig{
		Hostname:   nodeName,
		ControlURL: nexusURL, // From root.go
		AuthKey:    loginAuthkey,
	}

	srv, err := network.Connect(ctx, config)
	if err != nil {
		spinner.StopWithError(fmt.Sprintf("Failed to connect: %v", err))
		os.Exit(1)
	}

	ip, _ := srv.GetIPv4()
	spinner.StopWithSuccess(fmt.Sprintf("Connected as '%s'", nodeName))
	printNetworkSuccessInfo(nodeName, ip)

	// Disconnect cleanly so tsnet flushes its state files, then the
	// post-Close chown in Disconnect() fixes ownership for the non-root
	// worker process. The work command re-establishes its own connection.
	_ = network.Disconnect()
}

// runInteractiveLogin handles the interactive login flow
func runInteractiveLogin() {
	choice, key, err := nexus.GetNetworkChoice("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
		os.Exit(1)
	}

	var authKey string
	var nodeName string
	// servingNodeUID is set only when an interactive device-grant login submits
	// a CSR and the backend returns a validated enrollment bundle (#1062). It
	// switches this login's serving identity to the deterministic "node-<uid>"
	// name. Empty on every login today (backend companion aceteam#9576 unmerged),
	// so the serving identity stays the display hostname exactly as before.
	var servingNodeUID string

	switch choice {
	case nexus.NetChoiceVerified:
		// The GetNetworkChoice function already printed a success message.
		return
	case nexus.NetChoiceSkip:
		fmt.Println("Login skipped.")
		return
	case nexus.NetChoiceDevice:
		// Preflight: verify API is reachable before starting interactive auth
		if err := nexus.CheckAPIReachable(authServiceURL); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Cannot reach AceTeam API: %v\n", err)
			fmt.Fprintln(os.Stderr, "\nCheck your internet connection and try again.")
			os.Exit(1)
		}

		// Enroll the existing receipt-signer identity key: derive ONE CSR from
		// the machine-convergent identity store and submit it on every token
		// poll (#1062). Fail-open — a node that cannot build a CSR still logs in
		// exactly as before, just without CSR enrollment (the whole identity
		// path is fail-open until the fabric CA is activated).
		store := loginIdentityStore()
		csrPEM, csrPub, csrErr := generateLoginCSR(store)
		if csrErr != nil {
			// Do NOT log csrErr: it can be an os.PathError naming node.key,
			// and this codebase never logs private-key paths.
			Debug("login CSR unavailable (non-fatal, continuing without enrollment)")
			csrPEM, csrPub = "", nil
		}

		// Device authorization flow (carries the CSR when we have one)
		authResult, err := runDeviceAuthFlowWithCSR(authServiceURL, false, csrPEM)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			if nexus.IsNetworkError(err) {
				fmt.Fprintln(os.Stderr, "\nThe API became unreachable during authentication.")
				fmt.Fprintln(os.Stderr, "Check your network connection and try again.")
			} else {
				fmt.Fprintln(os.Stderr, "\nAlternative: Use 'citadel login --authkey <key>' for non-interactive login")
			}
			os.Exit(1)
		}
		authKey = authResult.Token.Authkey

		// Persist device config so the token survives across sessions
		if authResult.Token.DeviceAPIToken != "" {
			if err := saveDeviceConfigToFile(authResult.Token); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: Could not save device config: %v\n", err)
			}
		} else if authResult.Token.RedisURL != "" {
			if err := saveRedisURLToConfig(authResult.Token.RedisURL); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: Could not save Redis URL to config: %v\n", err)
			}
		}

		// Validate + persist any CSR-enrollment bundle. Fail CLOSED on a bad or
		// partial bundle: reject it (never persist a mismatched/partial cert,
		// never overwrite a known-good one) and fall back to the display
		// hostname so the node is honestly unverified rather than mis-enrolled.
		// Login itself still proceeds — the authkey is valid.
		outcome, bErr := persistLoginIdentityBundle(store, csrPub,
			authResult.Token.LeafPem, authResult.Token.ChainPem, authResult.Token.NodeUID)
		if bErr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Identity enrollment rejected: %v\n", bErr)
		} else {
			servingNodeUID = outcome.NodeUID
			if servingNodeUID != "" {
				if err := saveLoginNodeUID(servingNodeUID); err != nil {
					fmt.Fprintln(os.Stderr, "⚠️  Identity enrollment could not save serving identity.")
					servingNodeUID = ""
				}
			}
			if outcome.Persisted {
				fmt.Println("   Identity certificate stored.")
			}
		}

	case nexus.NetChoiceAuthkey:
		fmt.Println("--- Authenticating with authkey ---")
		authKey = key
	}

	// Use --node-name if provided, then saved hostname, then OS hostname
	if loginNodeName != "" {
		nodeName = loginNodeName
	} else if saved := getSavedHostname(); saved != "" {
		nodeName = saved
	} else {
		nodeName, _ = os.Hostname()
	}

	// Persist hostname for reboot stability. nodeName is the user's DISPLAY
	// hostname and is kept separate from the serving identity below.
	if err := saveHostnameToConfig(nodeName); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save hostname: %v\n", err)
	}

	// A CSR-enrolled login registers under the deterministic "node-<uid>"
	// serving identity the verifier maps to fabric_node_certs.node_uid; every
	// other login keeps its display hostname (servingNodeUID == "").
	servingHostname := servingIdentityHostname(servingNodeUID, nodeName)

	// Try to reclaim a stale node registered under the SERVING identity so the
	// coordination server does not suffix the new one (node-<uid>-1), which
	// would break the verifier's exact-match rule.
	if savedConfig := getDeviceConfigFromFile(); savedConfig != nil {
		reclaimStaleNodeByHostname(savedConfig.DeviceAPIToken, servingHostname)
		// When we just switched this node's serving identity to node-<uid>,
		// also sweep the prior registration under the display hostname so it
		// is not orphaned (scoped to CSR-enrolled login: servingHostname
		// differs from nodeName only then).
		if servingHostname != nodeName {
			reclaimStaleNodeByHostname(savedConfig.DeviceAPIToken, nodeName)
		}
	}

	// Disconnect any existing connection first
	_ = network.Logout()

	// Connect using tsnet with animated spinner
	spinner := whimsy.NewSimpleSpinner(whimsy.ConnectingMessages)
	spinner.Start()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config := network.ServerConfig{
		Hostname:   servingHostname,
		ControlURL: nexusURL,
		AuthKey:    authKey,
	}

	srv, err := network.Connect(ctx, config)
	if err != nil {
		spinner.StopWithError(fmt.Sprintf("Failed to connect: %v", err))
		os.Exit(1)
	}

	ip, _ := srv.GetIPv4()
	spinner.StopWithSuccess(fmt.Sprintf("Connected as '%s'", nodeName))
	printNetworkSuccessInfo(nodeName, ip)

	// Disconnect cleanly so tsnet flushes its state files, then the
	// post-Close chown in Disconnect() fixes ownership for the non-root
	// worker process. The work command re-establishes its own connection.
	_ = network.Disconnect()
}

func init() {
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().StringVar(&loginAuthkey, "authkey", "", "Pre-generated authkey for non-interactive login")
	loginCmd.Flags().StringVar(&loginNodeName, "node-name", "", "Override the node name (defaults to hostname)")
}
