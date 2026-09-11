// cmd/egress_relay_serve.go
//
// citadel #1033: relay-only mode. `citadel work --egress-relay` starts the
// mesh egress-relay listener, but `citadel work` ALSO requires a reachable
// Redis job source: it retries then EXITS on connect failure, taking the relay
// down with it. So a node behind a restrictive firewall could not be a PURE
// egress exit without also being a full, Redis-connected worker.
//
// `citadel egress-relay serve` closes that gap: a long-running foreground
// command that connects THIS process to the AceTeam Network, starts the exact
// same egress-relay listener `citadel work` starts (via the shared
// startEgressRelayListener in cmd/egress_relay_server.go -- no second copy of
// the SOCKS5/policy/same-org-authz wiring), and blocks until SIGINT/SIGTERM.
// It uses NO Redis, NO job source, and NO worker loop -- that is the whole
// point. The security posture (mesh-only, same-org verified peers,
// deny-LAN-by-default) is identical to the worker's relay because it IS the
// worker's relay implementation.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
)

var egressRelayServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run as a pure egress relay (no worker, no Redis) until interrupted",
	Long: `Run this node as a PURE mesh egress relay: it joins the AceTeam Network,
starts the same SOCKS5 egress-relay listener 'citadel work --egress-relay'
starts, and stays up until Ctrl-C -- with NO Redis job source and NO worker
loop. This is for a headless node behind a restrictive firewall that should be
an egress exit for other same-org nodes without also being a full job worker
(citadel #1033).

Unlike the 'citadel work' auto-start path, this command force-enables the relay
(running it IS the intent), so it does NOT consult the 'egress-relay enable'
toggle. The LAN/mesh deny-list still defaults ON and is only relaxed via
'citadel egress-relay allow-lan on' or CITADEL_EGRESS_ALLOW_LAN -- there is
deliberately no flag to force it off.

The node must already be logged in ('citadel login' or 'citadel init'); the
relay is mesh-only and needs a network identity.`,
	Args: cobra.NoArgs,
	RunE: runEgressRelayServe,
}

// runEgressRelayServe is the thin CLI entry: it wires the REAL dependencies
// (network state/verify probes, a real signal channel) into the testable core
// egressRelayServe. Kept intentionally minimal so the core -- including the
// not-logged-in error path and the happy start-then-shutdown path -- is
// exercisable without touching the live mesh (which on a machine that also
// runs a real node would reconnect using that node's identity).
func runEgressRelayServe(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	return egressRelayServe(ctx, cancel, network.HasState, network.VerifyOrReconnect, sigs)
}

// egressRelayServe is the relay-only mode's testable core. It:
//  1. confirms the node is on the mesh -- connecting via the SAME path
//     `citadel work` uses (VerifyOrReconnect, with recoverStaleVPN on stale
//     state); a not-logged-in node gets a clear, actionable error,
//  2. starts the shared egress-relay listener (startEgressRelayListener),
//  3. blocks until a signal arrives on sigs, then cancels ctx (which closes
//     the relay listener and lets its Serve goroutine return) and returns.
//
// Its dependencies are enumerated in the signature and NONE of them is a Redis
// client or job source -- that structural absence is the #1033 regression this
// command exists to prevent (see TestEgressRelayServe_* for the pinning).
func egressRelayServe(
	ctx context.Context,
	cancel context.CancelFunc,
	hasState func() bool,
	verify func(context.Context) (bool, error),
	sigs <-chan os.Signal,
) error {
	if !hasState() {
		return errors.New("this node is not connected to the AceTeam Network.\n" +
			"  The egress relay is mesh-only and needs a network identity -- run 'citadel login'\n" +
			"  (or 'citadel init') first, then re-run 'citadel egress-relay serve'.")
	}

	// Same network-connect path as runWork (cmd/work.go): VerifyOrReconnect,
	// and on ErrStaleState fall through to recoverStaleVPN (which fetches a
	// fresh authkey and reconnects). Both are file/const-driven -- no Redis.
	connected, err := verify(ctx)
	if err != nil && errors.Is(err, network.ErrStaleState) {
		Log("network state is stale after retries, attempting auto-recovery...")
		fmt.Println("   - VPN state is stale, attempting auto-recovery...")

		deviceConfig := getDeviceConfigFromFile()
		apiBaseURL := authServiceURL
		if deviceConfig != nil && deviceConfig.APIBaseURL != "" {
			apiBaseURL = deviceConfig.APIBaseURL
		}
		result := recoverStaleVPN(ctx, deviceConfig, getWorkHostname(), apiBaseURL)
		connected = result.Connected
		if !result.Connected {
			msg := "could not restore the AceTeam Network connection automatically"
			if result.Err != nil {
				msg = fmt.Sprintf("%s: %v", msg, result.Err)
			}
			return fmt.Errorf("%s.\n"+
				"  Run 'citadel reconnect' or 'citadel login --authkey <key>' to fix, then re-run.", msg)
		}
		if result.IPPreserved {
			fmt.Println("   - VPN reconnected (IP preserved)")
		} else {
			fmt.Println("   - VPN reconnected (fresh state)")
		}
	} else if err != nil {
		return fmt.Errorf("failed to connect to the AceTeam Network: %w", err)
	}

	if !connected || !egressRelayIsConnected() {
		return errors.New("could not establish an AceTeam Network connection.\n" +
			"  Run 'citadel reconnect' or 'citadel login --authkey <key>', then re-run 'citadel egress-relay serve'.")
	}

	fmt.Printf("Starting egress relay (relay-only mode, no worker/Redis) on mesh port %d...\n", services.EgressRelayPort)
	if _, err := startEgressRelayListener(ctx); err != nil {
		return fmt.Errorf("failed to start the egress relay: %w", err)
	}
	fmt.Println("   - Relay-only mode: no job source, no Redis, no worker loop. Press Ctrl-C to stop.")

	// Block until interrupted. Teardown is a single ctx cancel: it closes the
	// relay listener (egressrelay.Serve returns nil on ctx.Done) and the
	// process's userspace network connection is released on exit -- there is no
	// docker-compose teardown that can hang, so unlike runWork this needs no
	// grace-period force-exit watchdog (issue #312). We deliberately do NOT
	// network.Logout(): like `citadel work`, the node's identity must persist
	// across restarts.
	<-sigs
	fmt.Println("\n   - Received shutdown signal; stopping egress relay...")
	cancel()
	return nil
}

func init() {
	egressRelayCmd.AddCommand(egressRelayServeCmd)
}
