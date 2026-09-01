// cmd/pairing_code.go
//
// `citadel pairing-code` (citadel #659 P1, docs/design-pairing-display.md
// §9.2/§14) is the headless-fleet answer to the P0 console renderer
// (internal/pairingdisplay): the realistic majority of nodes have no VT to
// write a code to at all, so this is a short-lived CLI invocation, run by a
// human with their OWN independent access to the node (SSH key, physical
// login), that reads back whatever pairing code the long-running
// `citadel work`/control-center process is CURRENTLY holding.
//
// # Why this actually observes the running worker's state, not a fresh
// empty singleton
//
// The pending code lives in the memory of a DIFFERENT, long-running
// process's pairingdisplay.Manager -- this CLI invocation's own
// pairingdisplay.Get() singleton is fresh and empty and is deliberately
// never consulted here. Instead, pairingdisplay.RequestPendingCode dials a
// Unix socket the worker's Manager opens at
// <network.GetNodeConfigDir()>/pairing.sock (see socket.go's package doc)
// -- the SAME machine-convergent directory Configure() points the worker's
// Manager at (the #383/#845 rule), so a root `citadel work` and this
// (possibly non-root, hence `sudo citadel pairing-code`) invocation resolve
// the identical socket path. The 0600 file mode is the real access-control
// boundary (design doc §10.4's actor-equivalence) -- the client-side TTY
// gate below is the doc's own "cosmetic" guard against casual scripting,
// not a security measure.
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/pairingdisplay"
	"github.com/aceteam-ai/citadel-cli/internal/tui"
	"github.com/spf13/cobra"
)

// pairingCodeSocketTimeout bounds the local Unix-socket round trip. Dialing
// and reading from a local socket resolves near-instantly when a worker is
// listening; this only guards against an unexpectedly wedged worker.
const pairingCodeSocketTimeout = 3 * time.Second

var pairingCodeJSON bool

var pairingCodeCmd = &cobra.Command{
	Use:   "pairing-code",
	Short: "Show the currently pending node:exec pairing code (headless-fleet pull command)",
	Long: `citadel pairing-code prints the node:exec pairing code the platform most
recently pushed to this node via SHOW_PAIRING_CODE, if one is still pending
and unexpired -- the answer for a node with no attached screen for the
console renderer (P0) to write to (see docs/design-pairing-display.md §9.2).

It reads the code from the currently-running 'citadel work' (or control
center) process over a local, same-user-only Unix socket -- there is
nothing to retrieve if no such process is running, or if nothing is
currently pending. This command never talks to the AceTeam backend and
never touches the mesh network.

The requesting agent that originated the grant cannot run this command to
obtain the code for itself: node:exec is exactly the capability being
escalated, so by definition it has no shell on this node yet. Only a human
with independent access (their own SSH key, physical login, or an operator
credential the agent doesn't have) can invoke it.`,
	Example: `  citadel pairing-code
  sudo citadel pairing-code          # when the worker runs as root
  citadel pairing-code --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runPairingCode(cmd)
	},
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(pairingCodeCmd)
	pairingCodeCmd.Flags().BoolVar(&pairingCodeJSON, "json", false, "Output as JSON, for scripting")
}

func runPairingCode(cmd *cobra.Command) error {
	// The TTY gate is the design doc's own "cosmetic" guard (§9.2): it
	// discourages a script or an agent's own subprocess call from silently
	// harvesting the human-formatted output, but is NOT the real security
	// boundary (the socket's 0600 file mode is -- see this file's package
	// doc). --json is an explicit, deliberate request for machine-readable
	// output and bypasses it; the default human-formatted path still
	// refuses outside a real terminal.
	if !pairingCodeJSON && !tui.IsTTY() {
		return fmt.Errorf("citadel pairing-code must be run from an interactive terminal (use --json to allow scripted output)")
	}

	info, err := pairingdisplay.RequestPendingCode(network.GetNodeConfigDir(), pairingCodeSocketTimeout)
	if err != nil {
		return fmt.Errorf("read pending pairing code: %w", err)
	}
	return renderPairingCode(cmd.OutOrStdout(), info, pairingCodeJSON)
}

// renderPairingCode is the pure, unit-testable half of runPairingCode: given
// an already-fetched PendingCodeInfo (real, over the socket, or a
// hand-built one in a test), it decides what to print. Split out so the
// output-formatting logic (pending/expired/none, --json shape) is testable
// without a live worker process or network.GetNodeConfigDir()'s
// machine-dependent resolution -- mirrors cmd/whoami.go's
// gatherIdentity/renderIdentity split.
func renderPairingCode(out io.Writer, info pairingdisplay.PendingCodeInfo, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	if !info.Pending {
		fmt.Fprintln(out, "No pending pairing code.")
		return nil
	}

	fmt.Fprintln(out, "Pending node:exec pairing code:")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  Code:       %s\n", info.Code)
	if info.RequestedBy != "" {
		fmt.Fprintf(out, "  Requested:  %s\n", info.RequestedBy)
	}
	fmt.Fprintf(out, "  Valid for:  %s (until %s)\n",
		formatRemainingTTL(info.TTLSeconds), info.ExpiresAt.Local().Format(time.RFC1123))
	return nil
}

func formatRemainingTTL(seconds int) string {
	if seconds <= 0 {
		return "less than a second"
	}
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return d.String()
	}
	return d.Round(time.Second).String()
}
