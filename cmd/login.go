// cmd/login.go
package cmd

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/nodesession"
	"github.com/aceteam-ai/citadel-cli/internal/tui/whimsy"
	"github.com/spf13/cobra"
)

var (
	loginAuthkey   string
	loginNodeName  string
	loginNewDevice bool
)

type persistedFabricCredentials uint8

const (
	fabricCredentialsNone persistedFabricCredentials = iota
	fabricCredentialsAPI
	fabricCredentialsRedis
)

type loginNetworkChoiceFn func(string) (nexus.NetworkChoice, string, error)

func validateLoginOptions(authkey string, newDevice bool) error {
	if authkey != "" && newDevice {
		return fmt.Errorf("--authkey and --new-device cannot be used together")
	}
	return nil
}

// selectLoginNetworkChoice makes --new-device authoritative. In particular,
// do not let a healthy/stale existing mesh short-circuit the explicit request
// before device authorization can send force_new to the auth service. The old
// mesh state is left untouched until the ordinary post-authorization path.
func selectLoginNetworkChoice(newDevice bool, choose loginNetworkChoiceFn) (nexus.NetworkChoice, string, error) {
	if newDevice {
		return nexus.NetChoiceDevice, "", nil
	}
	return choose("")
}

// canKeepFabricEnrollmentWithoutMesh is deliberately narrow: only a
// self-hosted interactive device grant can enroll the fabric side independently
// of the mesh, and only after one of its fabric credential writes succeeded.
// Managed aceteam.ai login retains its existing fail-closed mesh requirement.
func canKeepFabricEnrollmentWithoutMesh(choice nexus.NetworkChoice, creds persistedFabricCredentials, authURL, controlURL string) bool {
	return choice == nexus.NetChoiceDevice &&
		creds != fabricCredentialsNone &&
		isSelfHostedEndpointPair(authURL, controlURL)
}

func isSelfHostedEndpointPair(authURL, controlURL string) bool {
	for _, raw := range []string{authURL, controlURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil ||
			(u.Scheme != "https" && u.Scheme != "http") {
			return false
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host == "aceteam.ai" || strings.HasSuffix(host, ".aceteam.ai") {
			return false
		}
	}
	return true
}

func (c persistedFabricCredentials) transportName() string {
	if c == fabricCredentialsRedis {
		return "direct Redis"
	}
	return "the fabric API"
}

// persistFabricEnrollmentWithoutMesh makes the partial enrollment durable
// before login reports success. Unlike the ordinary connected path, none of
// these writes can be best-effort: without the CA or Nexus URL an unattended
// worker may not reach this self-hosted tenant, and without valid session
// state a later bare invocation cannot honor the operator's preserved intent.
func persistFabricEnrollmentWithoutMesh(
	authURL, controlURL, nodeConfigDir string,
	persistTrust func(string, string) error,
	persistControlURL func(string) error,
	persistSession func(string, bool) (nodesession.Config, error),
) (nodesession.Config, error) {
	if err := persistTrust(authURL, controlURL); err != nil {
		return nodesession.Config{}, fmt.Errorf("persist control-plane trust: %w", err)
	}
	if err := persistControlURL(controlURL); err != nil {
		return nodesession.Config{}, fmt.Errorf("persist Nexus URL: %w", err)
	}
	cfg, err := persistSession(nodeConfigDir, true)
	if err != nil {
		return nodesession.Config{}, fmt.Errorf("persist node session mode: %w", err)
	}
	return cfg, nil
}

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
		if err := validateLoginOptions(loginAuthkey, loginNewDevice); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

		// Refuse an explicit --nexus that differs from the control plane this
		// node is already enrolled against (citadel-cli#1110).
		if err := refuseNexusFlagMismatch(cmd); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}

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
	// Best-effort, matching the sibling saveHostnameToConfig above: a corrupt or
	// unwritable config.yaml must not hard-fail an --authkey login that would
	// otherwise succeed. A stale login_node_uid only affects the CSR-login
	// serving name, which the authkey path does not use.
	if err := clearLoginNodeUID(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not clear prior login serving identity: %v\n", err)
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
	if err := persistPrivateCATrust(authServiceURL, nexusURL); err != nil {
		_ = network.Disconnect()
		spinner.StopWithError(err.Error())
		os.Exit(1)
	}

	// Persist the control URL we enrolled against (citadel-cli#1110) so later
	// reconnects target this control plane, not the compiled-in default.
	persistNexusURLBestEffort(nexusURL)
	if _, err := nodesession.LoadOrInitialize(network.GetNodeConfigDir(), false); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist session mode: %v\n", err)
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
	choice, key, err := selectLoginNetworkChoice(loginNewDevice, nexus.GetNetworkChoice)
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

	// persistedCreds records which independent fabric transport a device-auth
	// login saved. A later mesh failure can preserve that enrollment without
	// claiming a transport the token did not configure.
	var persistedCreds persistedFabricCredentials

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
		authResult, err := runDeviceAuthFlowWithCSR(authServiceURL, loginNewDevice, csrPEM)
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
			} else {
				persistedCreds = fabricCredentialsAPI
			}
		} else if authResult.Token.RedisURL != "" {
			if err := saveRedisURLToConfig(authResult.Token.RedisURL); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: Could not save Redis URL to config: %v\n", err)
			} else {
				persistedCreds = fabricCredentialsRedis
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
	// Reconcile the machine-wide serving name before registration. A valid CSR
	// bundle selects node-{uid}; an authkey, legacy no-bundle, or rejected
	// bundle selects the display hostname and clears any prior enrollment UID.
	if servingNodeUID != "" {
		if err := saveLoginNodeUID(servingNodeUID); err != nil {
			fmt.Fprintln(os.Stderr, "Error: could not save login serving identity.")
			os.Exit(1)
		}
	} else if err := clearLoginNodeUID(); err != nil {
		fmt.Fprintln(os.Stderr, "Error: could not clear prior login serving identity.")
		os.Exit(1)
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
		// A device-auth enrollment with independently persisted fabric
		// credentials need not be discarded just because its mesh connection
		// failed. Before reporting that partial success, make any explicitly
		// supplied private CA durable for the unattended worker. An authkey-only
		// login, or a failed credential write, still fails hard because it has no
		// usable non-mesh enrollment to preserve.
		if canKeepFabricEnrollmentWithoutMesh(choice, persistedCreds, authServiceURL, nexusURL) {
			spinner.StopWithError(fmt.Sprintf("Mesh connection failed: %v", err))
			sessionCfg, persistErr := persistFabricEnrollmentWithoutMesh(
				authServiceURL,
				nexusURL,
				network.GetNodeConfigDir(),
				persistPrivateCATrust,
				saveNexusURLToConfig,
				nodesession.LoadOrInitialize,
			)
			if persistErr != nil {
				fmt.Fprintf(os.Stderr, "Error: fabric credentials were saved, but enrollment state could not be persisted: %v\n", persistErr)
				os.Exit(1)
			}
			fmt.Fprintln(os.Stderr, "\n⚠️  Fabric enrollment completed, but the mesh connection failed.")
			fmt.Fprintf(os.Stderr, "   Credentials are saved for %s.\n", persistedCreds.transportName())
			if sessionCfg.Mode == nodesession.Worker {
				fmt.Fprintln(os.Stderr, "   Worker mode is active; the node can serve fabric jobs without the mesh.")
				fmt.Fprintln(os.Stderr, "   The worker will retry the mesh when it starts.")
			} else {
				fmt.Fprintln(os.Stderr, "   Existing presence-only mode was preserved; run 'citadel work' explicitly to serve jobs.")
			}
			return
		}
		spinner.StopWithError(fmt.Sprintf("Failed to connect: %v", err))
		os.Exit(1)
	}
	if err := persistPrivateCATrust(authServiceURL, nexusURL); err != nil {
		_ = network.Disconnect()
		spinner.StopWithError(err.Error())
		os.Exit(1)
	}

	// Persist the control URL we enrolled against (citadel-cli#1110) so later
	// reconnects target this control plane, not the compiled-in default.
	persistNexusURLBestEffort(nexusURL)
	if _, err := nodesession.LoadOrInitialize(network.GetNodeConfigDir(), choice == nexus.NetChoiceDevice); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not persist session mode: %v\n", err)
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
	loginCmd.Flags().BoolVar(&loginNewDevice, "new-device", false, "Force fresh registration, ignoring any existing machine mapping")
}
