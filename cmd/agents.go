// cmd/agents.go
//
// `citadel agents probe` — discovery-only detection of vendor coding-agent
// CLIs (Claude Code, Codex, Gemini CLI, OpenCode) already installed and
// authenticated on this node's own PATH (issue aceteam#8993, DoR-v2 slice
// S1: "drive installed vendor coding agents on user hardware, wrapped in
// AEP receipts").
//
// This is deliberately a sibling of internal/capabilities (fabric/hardware
// capability detection for queue routing) and internal/discovery (mesh peer
// discovery), not a bolt-on to either: local vendor-CLI inspection is its
// own concern with its own read-only guardrail (never executes an agent
// turn, never makes a network call — see internal/agentsprobe's package
// doc). Later DoR-v2 slices (S4/S5) build the driving adapter this probe
// only reports the presence of.
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/agentsprobe"
	"github.com/spf13/cobra"
)

var agentsProbeJSON bool

// agentsProbeTimeout bounds the whole probe (one bounded --version exec per
// vendor, sequential); generous relative to each vendor's own
// internal/agentsprobe.probeTimeout so a single slow/hung binary can't stall
// the others indefinitely.
const agentsProbeTimeout = 30 * time.Second

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Discover vendor coding agents installed on this node",
	Long: `Detects vendor coding-agent CLIs already installed and authenticated on this
node, for the node's own account — not a container or binary AceTeam ships.

This is discovery only. It never spawns, drives, or drives a turn of any
vendor agent.`,
}

var agentsProbeCmd = &cobra.Command{
	Use:   "probe",
	Short: "List vendor coding agents detected on this node",
	Long: `Probes PATH for known vendor coding-agent binaries (claude, codex, gemini,
opencode), reads each installed binary's --version output, and checks local
credential files for a best-effort authed/unauthenticated/unknown signal.

Read-only: never executes an agent turn, never makes a network call. Auth
state is inferred from local credential-file presence and shallow structure
only — credential VALUES are never read into the output.`,
	Example: `  # Table output
  citadel agents probe

  # JSON output
  citadel agents probe --json`,
	Run: runAgentsProbe,
}

func runAgentsProbe(cmd *cobra.Command, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), agentsProbeTimeout)
	defer cancel()

	agents := agentsprobe.Probe(ctx)

	if agentsProbeJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(agents); err != nil {
			badColor.Fprintf(os.Stderr, "Failed to encode JSON: %v\n", err)
			os.Exit(1)
		}
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tINSTALLED\tVERSION\tAUTHED\tADAPTER_CLASS")
	fmt.Fprintln(w, "----\t---------\t-------\t------\t-------------")
	for _, a := range agents {
		version := a.Version
		if version == "" {
			version = "-"
		}
		authed := string(a.Authed)
		if authed == "" {
			authed = "-"
		}
		fmt.Fprintf(w, "%s\t%v\t%s\t%s\t%s\n", a.Name, a.Installed, version, authed, a.AdapterClass)
	}
	w.Flush()
}

func init() {
	rootCmd.AddCommand(agentsCmd)
	agentsCmd.AddCommand(agentsProbeCmd)

	agentsProbeCmd.Flags().BoolVar(&agentsProbeJSON, "json", false, "Output in JSON format")
}
