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
// the identical socket path.
//
// # POSIX only -- Windows fails honestly, never silently
//
// This mechanism is Linux/macOS only. On those platforms the socket's 0600
// file mode is a real, kernel-enforced access-control boundary (design doc
// §10.4's actor-equivalence) -- the client-side TTY gate below is, per the
// design doc itself, a "cosmetic" guard against casual scripting on top of
// that, not a security measure in its own right. On Windows, `os.Chmod`
// does not set a real ACL, so there IS no such boundary there; rather than
// pretend otherwise, `pairingdisplay.RequestPendingCode` never opens a
// socket at all on Windows and returns `pairingdisplay.ErrUnsupportedPlatform`
// -- surfaced below as a clear, distinct error/JSON shape, never as a
// silent "no pending code" (which would be indistinguishable from "nothing
// is pending" and could mislead an operator into thinking there's nothing
// to retrieve when there might be). See internal/pairingdisplay/socket_windows.go.
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

Linux/macOS only for now: on Windows, os.Chmod does not provide a real
access-control boundary for this mechanism, so it is disabled there rather
than shipped with a false security guarantee -- this command reports a
clear "not supported on this platform yet" error on Windows instead of a
silent "no pending code".

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
	// boundary -- the socket's 0600 file mode is, and ONLY on POSIX (see
	// this file's package doc for why Windows has no equivalent boundary
	// and gets no socket at all). --json is an explicit, deliberate request
	// for machine-readable output and bypasses this TTY check; the default
	// human-formatted path still refuses outside a real terminal.
	if !pairingCodeJSON && !tui.IsTTY() {
		return fmt.Errorf("citadel pairing-code must be run from an interactive terminal (use --json to allow scripted output)")
	}

	info, err := pairingdisplay.RequestPendingCode(network.GetNodeConfigDir(), pairingCodeSocketTimeout)
	if err != nil {
		wrapped := fmt.Errorf("read pending pairing code: %w", err)
		if pairingCodeJSON {
			// A script parsing --json output must always get a JSON body on
			// stdout, even on failure -- including the "unsupported
			// platform" case (ErrUnsupportedPlatform), which is a NORMAL,
			// expected outcome on Windows, not an exceptional one. This
			// still returns the error too (cobra prints it to stderr and
			// the process exits non-zero), so a caller checking the exit
			// code sees a real failure; a caller only reading stdout sees a
			// well-formed, parseable explanation rather than a bare Go
			// error string mixed into what would otherwise be JSON.
			return renderPairingCodeError(cmd.OutOrStdout(), wrapped)
		}
		return wrapped
	}
	return renderPairingCode(cmd.OutOrStdout(), info, pairingCodeJSON)
}

// renderPairingCodeError writes a machine-readable {"pending":false,
// "error":"..."} body to out and returns err unchanged, so the command
// still exits non-zero (cobra's default stderr "Error: ..." line still
// fires too) while stdout stays valid JSON.
func renderPairingCodeError(out io.Writer, err error) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"pending": false,
		"error":   err.Error(),
	})
	return err
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
