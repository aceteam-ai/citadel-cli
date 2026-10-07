// cmd/reconnect.go
// Manual VPN reconnection command for recovering from stale network state.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/spf13/cobra"
)

var reconnectForce bool

// Indirection seams so the recovery path is testable without touching real
// network state. On a live node GetStateDir() resolves to that node's live
// tailscaled.state, so a test that reached the real ClearState()/Connect() would
// wipe the node's identity (issue #1235). Tests swap these for stubs and assert
// clearStateFn is never called on an unattended (non --force) recovery.
var (
	fetchFreshAuthkeyFn    = network.FetchFreshAuthkey
	reconnectWithAuthKeyFn = network.ReconnectWithAuthKey
	connectFn              = network.Connect
	clearStateFn           = network.ClearState
	getStateDirFn          = network.GetStateDir
	hasStateFn             = network.HasState
	resolveControlURLFn    = network.ResolveControlURL
	reclaimStaleNodeFn     = reclaimStaleNodeByHostname
)

var reconnectCmd = &cobra.Command{
	Use:   "reconnect",
	Short: "Recover a stale VPN connection",
	Long: `Clears stale VPN state and re-authenticates with the AceTeam Network.

When tsnet state becomes stale (expired/revoked Headscale key, corrupted
WireGuard state), the VPN cannot reconnect. This command automates recovery:

  1. Verifies VPN is actually broken (exits early if already connected)
  2. Fetches a fresh authkey using the device API token
  3. Attempts reconnect with existing state (preserves IP address)
  4. Falls back to clearing state + fresh connect if needed

Requires a device API token (stored during 'citadel login' or 'citadel init').

Use --force to skip the verify step and go straight to clear + reconnect.`,
	Example: `  # Reconnect (tries IP-preserving reconnect first)
  citadel reconnect

  # Force clear state and reconnect from scratch
  citadel reconnect --force`,
	Run: func(cmd *cobra.Command, args []string) {
		runReconnect()
	},
}

func runReconnect() {
	ctx := context.Background()

	// If already connected and not forcing, exit early
	if !reconnectForce && network.IsGlobalConnected() {
		fmt.Println("VPN is already connected.")
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if status, err := network.GetGlobalStatus(ctx); err == nil && status.Connected {
			fmt.Printf("  Hostname: %s\n", status.Hostname)
			fmt.Printf("  IP:       %s\n", status.IPv4)
		}
		return
	}

	// Load device config for API token
	deviceConfig := getDeviceConfigFromFile()
	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		fmt.Fprintln(os.Stderr, "Error: No device API token found.")
		fmt.Fprintln(os.Stderr, "Run 'citadel login' or 'citadel init' to authenticate first.")
		os.Exit(1)
	}

	apiBaseURL := deviceConfig.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = authServiceURL
	}

	// Diagnostic logging is wired once in root.go's PersistentPreRun (#662),
	// so every command gets it -- not just the ones that remembered.

	var result VPNRecoveryResult
	if reconnectForce {
		// Skip verify, clear state first, then use shared recovery
		fmt.Println("Forcing VPN reconnect (clearing state)...")
		if err := clearStateFn(); err != nil {
			Debug("failed to clear state: %v", err)
		}
		result = recoverStaleVPN(ctx, deviceConfig, getWorkHostname(), apiBaseURL, true)
	} else {
		// Normal flow: verify first, then recover if stale
		result = attemptVPNRecovery(ctx, deviceConfig, getWorkHostname(), apiBaseURL)
	}

	if result.Connected {
		fmt.Println("VPN connection recovered successfully.")
		if result.IPPreserved {
			fmt.Println("  IP address was preserved.")
		} else {
			fmt.Println("  Note: IP address may have changed (fresh state).")
		}
		// Print current status
		statusCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if status, err := network.GetGlobalStatus(statusCtx); err == nil && status.Connected {
			fmt.Printf("  Hostname: %s\n", status.Hostname)
			fmt.Printf("  IP:       %s\n", status.IPv4)
		}
		_ = network.Disconnect()
	} else {
		fmt.Fprintln(os.Stderr, "VPN recovery failed.")
		if result.Err != nil {
			fmt.Fprintf(os.Stderr, "  Error: %v\n", result.Err)
		}
		fmt.Fprintln(os.Stderr, "\nTry 'citadel reconnect --force' or 'citadel login --authkey <key>'.")
		os.Exit(1)
	}
}

// VPNRecoveryResult holds the outcome of a VPN recovery attempt.
type VPNRecoveryResult struct {
	// Connected is true if VPN was successfully established.
	Connected bool
	// IPPreserved is true if reconnect kept the existing IP (state was reused).
	IPPreserved bool
	// Err is the last error encountered, if any.
	Err error
}

// attemptVPNRecovery verifies VPN health first, then recovers if stale.
// Use this when the caller has NOT already checked VPN state (e.g.
// 'citadel reconnect'). If the caller has already called
// VerifyOrReconnect and knows the state is stale, call recoverStaleVPN
// directly to avoid a redundant ~10s timeout.
func attemptVPNRecovery(ctx context.Context, deviceConfig *DeviceConfig, hostname, apiBaseURL string) VPNRecoveryResult {
	// Verify VPN is actually broken
	connected, err := network.VerifyOrReconnect(ctx)
	if err == nil && connected {
		return VPNRecoveryResult{Connected: true, IPPreserved: true}
	}
	if err != nil && !errors.Is(err, network.ErrStaleState) {
		return VPNRecoveryResult{Err: fmt.Errorf("unexpected network error: %w", err)}
	}

	// State is stale (or no state) -- delegate to core recovery. This is the
	// unattended 'citadel reconnect' path: never churn node identity here.
	return recoverStaleVPN(ctx, deviceConfig, hostname, apiBaseURL, false)
}

// recoverStaleVPN performs the actual VPN recovery: fetch a fresh authkey,
// try IP-preserving reconnect, fall back to clear + fresh connect.
//
// Called by 'citadel work' (which already verified via VerifyOrReconnect)
// and by attemptVPNRecovery (which verifies first on behalf of
// 'citadel reconnect'). Also used by the --force path to avoid
// duplicating fetch+connect logic.
func recoverStaleVPN(ctx context.Context, deviceConfig *DeviceConfig, hostname, apiBaseURL string, allowChurn bool) VPNRecoveryResult {
	Log("VPN state is stale, attempting recovery (state_dir=%s, has_state=%v, allow_churn=%v)...",
		getStateDirFn(), hasStateFn(), allowChurn)

	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		Log("no device API token available for auto-recovery")
		return VPNRecoveryResult{
			Err: fmt.Errorf("no device API token available for auto-recovery"),
		}
	}

	// Fetch a fresh authkey from the platform
	Log("requesting fresh authkey from %s", apiBaseURL)
	freshKey, fetchErr := fetchFreshAuthkeyFn(ctx, apiBaseURL, deviceConfig.DeviceAPIToken)
	if fetchErr != nil {
		Log("failed to fetch fresh authkey: %v", fetchErr)
		return VPNRecoveryResult{
			Err: fmt.Errorf("could not fetch fresh authkey: %w", fetchErr),
		}
	}

	// Attempt 1: reconnect with existing state + fresh key (preserves IP)
	Log("attempting reconnect with existing state (IP-preserving)...")
	if ok, reconnErr := reconnectWithAuthKeyFn(ctx, freshKey); reconnErr == nil && ok {
		Log("reconnected with existing state (IP preserved)")
		return VPNRecoveryResult{Connected: true, IPPreserved: true}
	} else {
		Log("IP-preserving reconnect failed: %v", reconnErr)
	}

	// Attempt 1 failed. Re-registering from scratch here would discard the
	// persisted machine key and mint a BRAND-NEW node (new id, new IP, new
	// device key), orphaning every capability bound to the old node id
	// (WhatsApp/WeChat creds, Files, node:exec grants, the per-node job stream)
	// -- issue #1235. That is only ever acceptable as an EXPLICIT operator
	// action, never on an unattended path. Unless churn was explicitly
	// authorized (citadel reconnect --force), refuse and leave the persisted
	// identity intact so a later reattach (or the control-plane fix) recovers
	// the SAME node.
	if !allowChurn {
		Log("refusing to churn node identity on an unattended recovery; leaving persisted state intact")
		return VPNRecoveryResult{
			Err: fmt.Errorf("could not re-establish the VPN with the existing node identity, and " +
				"automatic identity churn is disabled: the control plane may be unreachable or the " +
				"node key may need re-authorization. Re-run 'citadel reconnect --force' to re-register " +
				"as a new node (this mints a new node id and requires re-provisioning capabilities bound " +
				"to the old one), or 'citadel login --authkey <key>'"),
		}
	}

	// Attempt 2 (EXPLICIT CHURN, --force only): discard the persisted identity
	// and register as a new node. See warnIdentityChurn.
	warnIdentityChurn(hostname)

	// Reclaim the stale Headscale node so the dashboard doesn't accumulate
	// duplicate entries (issue #246). Best-effort.
	if hostname != "" {
		Log("reclaiming stale node '%s' before fresh connect...", hostname)
		reclaimStaleNodeFn(deviceConfig.DeviceAPIToken, hostname)
	}
	Log("clearing state for fresh connect...")
	if clearErr := clearStateFn(); clearErr != nil {
		Log("failed to clear network state: %v", clearErr)
	}

	// Attempt 1 may have spent the single-use authkey at the control plane, and
	// a spent single-use key on an expired node is the headscale #2434 panic/401
	// condition. Fetch a fresh key for the churn connect rather than reusing it.
	churnKey, churnKeyErr := fetchFreshAuthkeyFn(ctx, apiBaseURL, deviceConfig.DeviceAPIToken)
	if churnKeyErr != nil {
		Log("failed to fetch fresh authkey for churn connect: %v", churnKeyErr)
		return VPNRecoveryResult{Err: fmt.Errorf("could not fetch authkey for fresh connect: %w", churnKeyErr)}
	}

	freshCtx, freshCancel := context.WithTimeout(ctx, 30*time.Second)
	defer freshCancel()
	config := network.ServerConfig{
		Hostname:   hostname,
		ControlURL: resolveControlURLFn(),
		StateDir:   getStateDirFn(),
		AuthKey:    churnKey,
	}
	if _, connectErr := connectFn(freshCtx, config); connectErr != nil {
		Log("fresh connect also failed: %v", connectErr)
		return VPNRecoveryResult{Err: fmt.Errorf("all recovery attempts failed: %w", connectErr)}
	}
	Log("reconnected with fresh state (new IP)")
	return VPNRecoveryResult{Connected: true, IPPreserved: false}
}

// warnIdentityChurn emits a prominent, structured warning that the node is about
// to lose its persisted identity and re-register as a new node. This path runs
// only under an explicit 'citadel reconnect --force' (issue #1235): Headscale
// reattaches on a preserved machine key, so a new node is minted precisely
// because a fresh machine key is being presented. The churn orphans every
// capability bound to the old node id. Surfaced loudly so it is diagnosable.
func warnIdentityChurn(hostname string) {
	Log("IDENTITY CHURN (--force): discarding persisted identity and re-registering '%s' as a NEW node "+
		"(new fabric id + new mesh IP + new device key). Capabilities bound to the old node id "+
		"(WhatsApp/WeChat, Files, node:exec, per-node job stream) will need re-provisioning.", hostname)
	fmt.Fprintln(os.Stderr, "   - WARNING: node identity is being reset (new node id, new IP, new device key).")
	fmt.Fprintln(os.Stderr, "     Capabilities bound to the old node id (WhatsApp/WeChat, Files, node:exec) will")
	fmt.Fprintln(os.Stderr, "     need re-provisioning. This was requested explicitly via --force.")
}

func init() {
	rootCmd.AddCommand(reconnectCmd)
	reconnectCmd.Flags().BoolVar(&reconnectForce, "force", false, "Skip verification, clear state and reconnect from scratch")
}
