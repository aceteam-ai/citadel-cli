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

	// Wire up diagnostic logging
	network.SetLogf(Debug)

	var result VPNRecoveryResult
	if reconnectForce {
		// Skip verify, clear state first, then use shared recovery
		fmt.Println("Forcing VPN reconnect (clearing state)...")
		if err := network.ClearState(); err != nil {
			Debug("failed to clear state: %v", err)
		}
		result = recoverStaleVPN(ctx, deviceConfig, getWorkHostname(), apiBaseURL)
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

	// State is stale (or no state) -- delegate to core recovery
	return recoverStaleVPN(ctx, deviceConfig, hostname, apiBaseURL)
}

// recoverStaleVPN performs the actual VPN recovery: fetch a fresh authkey,
// try IP-preserving reconnect, fall back to clear + fresh connect.
//
// Called by 'citadel work' (which already verified via VerifyOrReconnect)
// and by attemptVPNRecovery (which verifies first on behalf of
// 'citadel reconnect'). Also used by the --force path to avoid
// duplicating fetch+connect logic.
func recoverStaleVPN(ctx context.Context, deviceConfig *DeviceConfig, hostname, apiBaseURL string) VPNRecoveryResult {
	Log("VPN state is stale, attempting recovery (state_dir=%s, has_state=%v)...",
		network.GetStateDir(), network.HasState())

	if deviceConfig == nil || deviceConfig.DeviceAPIToken == "" {
		Log("no device API token available for auto-recovery")
		return VPNRecoveryResult{
			Err: fmt.Errorf("no device API token available for auto-recovery"),
		}
	}

	// Fetch a fresh authkey from the platform
	Log("requesting fresh authkey from %s", apiBaseURL)
	freshKey, fetchErr := network.FetchFreshAuthkey(ctx, apiBaseURL, deviceConfig.DeviceAPIToken)
	if fetchErr != nil {
		Log("failed to fetch fresh authkey: %v", fetchErr)
		return VPNRecoveryResult{
			Err: fmt.Errorf("could not fetch fresh authkey: %w", fetchErr),
		}
	}

	// Attempt 1: reconnect with existing state + fresh key (preserves IP)
	Log("attempting reconnect with existing state (IP-preserving)...")
	if ok, reconnErr := network.ReconnectWithAuthKey(ctx, freshKey); reconnErr == nil && ok {
		Log("reconnected with existing state (IP preserved)")
		return VPNRecoveryResult{Connected: true, IPPreserved: true}
	} else {
		Log("IP-preserving reconnect failed: %v", reconnErr)
	}

	// Attempt 2: clear state and connect from scratch (new IP/hostname).
	//
	// THIS IS THE IDENTITY-CHURN PATH. Reaching here means the persisted machine
	// identity in tailscaled.state could not be re-authorized even with a fresh
	// authkey — so we are about to discard it and register as a brand-new node.
	// That mints a new fabric/Headscale node id, a new mesh IP, and (on the
	// backend) a new device key. The usual root cause is an EPHEMERAL Headscale
	// registration: when the node went offline, Headscale removed the ephemeral
	// node, so the persisted machine key no longer maps to any node and only a
	// fresh registration succeeds. The durable fix is backend PR #4584 (register
	// non-ephemerally) so an offline node is never removed; see aceteam #4583.
	// Warn loudly so this is diagnosable in the field rather than silent.
	warnIdentityChurn(hostname)

	// Before clearing, reclaim the stale Headscale node so the dashboard
	// doesn't accumulate duplicate entries on every restart (issue #246).
	if deviceConfig.DeviceAPIToken != "" && hostname != "" {
		Log("reclaiming stale node '%s' before fresh connect...", hostname)
		reclaimStaleNodeByHostname(deviceConfig.DeviceAPIToken, hostname)
	}
	Log("clearing state for fresh connect...")
	if clearErr := network.ClearState(); clearErr != nil {
		Log("failed to clear network state: %v", clearErr)
	}
	freshCtx, freshCancel := context.WithTimeout(ctx, 30*time.Second)
	defer freshCancel()
	config := network.ServerConfig{
		Hostname:   hostname,
		ControlURL: network.DefaultControlURL,
		StateDir:   network.GetStateDir(),
		AuthKey:    freshKey,
	}
	_, connectErr := network.Connect(freshCtx, config)
	if connectErr == nil {
		Log("reconnected with fresh state (new IP)")
		return VPNRecoveryResult{Connected: true, IPPreserved: false}
	}

	Log("fresh connect also failed: %v", connectErr)
	return VPNRecoveryResult{
		Err: fmt.Errorf("all recovery attempts failed: %w", connectErr),
	}
}

// warnIdentityChurn emits a prominent, structured warning that the node is about
// to lose its persisted identity and re-register as a new node. This is the
// symptom of an ephemeral Headscale registration (the node was removed while
// offline); the durable fix is non-ephemeral registration (backend #4584 /
// aceteam #4583). Surfacing it loudly makes the churn diagnosable in the field
// instead of a silent id/IP/device-key change.
func warnIdentityChurn(hostname string) {
	Log("IDENTITY CHURN: reusing persisted identity failed; re-registering '%s' as a NEW node "+
		"(new fabric id + new mesh IP + new device key). Likely cause: ephemeral Headscale "+
		"registration removed the node while offline. Durable fix: non-ephemeral registration "+
		"(aceteam #4583 / backend #4584). One-time migration: re-run 'citadel init' so this node "+
		"re-registers with a persistent key.", hostname)
	fmt.Fprintln(os.Stderr, "   - WARNING: node identity is being reset (new node id, new IP, new device key).")
	fmt.Fprintln(os.Stderr, "     Cause: the persisted identity could not be re-authorized (likely an ephemeral")
	fmt.Fprintln(os.Stderr, "     registration removed while offline). This node will keep working but appears as a")
	fmt.Fprintln(os.Stderr, "     new node. To stop recurring churn, re-run 'citadel init' to re-register persistently.")
}

func init() {
	rootCmd.AddCommand(reconnectCmd)
	reconnectCmd.Flags().BoolVar(&reconnectForce, "force", false, "Skip verification, clear state and reconnect from scratch")
}
