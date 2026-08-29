// cmd/module_reservations.go
//
// `citadel module reservations list|release` (docs/design-model-exclusivity.md
// §2.6) is the operator escape hatch for a job-scoped GPU reservation
// (internal/jobs.ServiceHandler.Reserve/ReserveExclusive, #832/aceteam#8248)
// that is stuck -- the process that created it is gone (crashed, SIGKILLed,
// or the design's §2.3(a) control-center/worklock race described in
// internal/jobs/model_exclusivity.go's package doc) or an explicit Release
// call itself keeps failing per-service. Both are thin wrappers over the
// already-tested ActiveReservations/Release primitives -- no new decision
// logic, just a CLI surface, mirroring `citadel module stop|start|restart`'s
// --dry-run/--expect-node posture (citadel#853).
package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/spf13/cobra"
)

var moduleReservationsCmd = &cobra.Command{
	Use:   "reservations",
	Short: "Inspect and manually release job-scoped GPU reservations (aceteam#8248/#8249)",
	Long: `A GPU reservation (created by 'citadel run --exclusive' or a local_run_exclusive
MCP tool call) durably evicts non-pinned services and restores them when
released. These commands are the manual escape hatch when that release
didn't happen automatically -- the reserving process crashed or was killed,
or a Release call itself keeps failing for one of the evicted services.`,
}

var reservationsExpectNode string

var moduleReservationsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active GPU reservations and the services each one evicted",
	Example: `  citadel module reservations list
  citadel module reservations list --expect-node my-test-node`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleReservationsList(cmd.Context())
	},
	SilenceUsage: true,
}

var reservationsDryRun bool

var moduleReservationsReleaseCmd = &cobra.Command{
	Use:   "release <jobID>",
	Short: "Release a GPU reservation, restoring the services it evicted",
	Long: `Restarts every service still tagged evicted_by_job==<jobID>, restores each
one's prior desired_status, and clears the reservation tag -- exactly what
'citadel run --exclusive' does automatically on Ctrl-C. Safe to call more
than once: a service that already came back (or was never evicted) is
skipped. <jobID> for an exclusive run is "exclusive:<service-name>" (see
'citadel module reservations list' to read it back, or reconstruct it
directly -- the format is a stable, documented contract).`,
	Example: `  citadel module reservations release exclusive:bonsai
  citadel module reservations release exclusive:bonsai --dry-run
  citadel module reservations release exclusive:bonsai --expect-node my-test-node`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleReservationsRelease(cmd.Context(), args[0])
	},
	SilenceUsage: true,
}

func init() {
	moduleCmd.AddCommand(moduleReservationsCmd)
	moduleReservationsCmd.AddCommand(moduleReservationsListCmd)
	moduleReservationsCmd.AddCommand(moduleReservationsReleaseCmd)

	for _, c := range []*cobra.Command{moduleReservationsListCmd, moduleReservationsReleaseCmd} {
		c.Flags().StringVar(&reservationsExpectNode, "expect-node", "", "Refuse to act unless the resolved node's identity (name, hostname, or mesh ID) matches this value")
	}
	moduleReservationsReleaseCmd.Flags().BoolVar(&reservationsDryRun, "dry-run", false, "Print what would be restored without doing it")
}

// resolveReservationsHandler applies the shared --expect-node gate (mirrors
// runModuleControl's, cmd/module_control.go) and the --node-dir safety guard
// (internal/jobs' ensureEmbeddedComposeFile only sees CITADEL_NODE_DIR via
// the environment, not the --node-dir flag -- see
// refuseIfReservationNodeDirUnsupported), then returns a ServiceHandler
// rooted at the resolved config dir.
func resolveReservationsHandler(ctx context.Context, cmdLabel string) (*jobs.ServiceHandler, error) {
	if err := refuseIfReservationNodeDirUnsupported(cmdLabel); err != nil {
		return nil, err
	}
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return nil, err
	}
	if reservationsExpectNode != "" {
		matched := expectNodeMatchesFast(manifest, reservationsExpectNode)
		var id NodeIdentity
		if !matched {
			id = gatherIdentity(ctx)
			matched = nodeIdentityMatches(id, reservationsExpectNode)
		}
		if !matched {
			return nil, fmt.Errorf("refusing to run %q: --expect-node %q does not match this node "+
				"(name=%q hostname=%q mesh-id=%q, resolved node dir=%s)",
				cmdLabel, reservationsExpectNode, id.NodeName, id.Hostname, id.HeadscaleNodeID, configDir)
		}
	}
	return jobs.NewServiceHandlerWithWorkspace(configDir, resolveWorkspaceDir()), nil
}

func runModuleReservationsList(ctx context.Context) error {
	handler, err := resolveReservationsHandler(ctx, "citadel module reservations list")
	if err != nil {
		return err
	}
	summaries, err := handler.ActiveReservations()
	if err != nil {
		return fmt.Errorf("list reservations: %w", err)
	}
	if len(summaries) == 0 {
		fmt.Println("No active GPU reservations.")
		return nil
	}
	fmt.Printf("%-32s  %s\n", "JOB ID", "EVICTED SERVICES")
	for _, s := range summaries {
		fmt.Printf("%-32s  %s\n", s.JobID, strings.Join(s.EvictedServices, ", "))
	}
	return nil
}

func runModuleReservationsRelease(ctx context.Context, jobID string) error {
	handler, err := resolveReservationsHandler(ctx, "citadel module reservations release")
	if err != nil {
		return err
	}

	if reservationsDryRun {
		summaries, err := handler.ActiveReservations()
		if err != nil {
			return fmt.Errorf("list reservations: %w", err)
		}
		fmt.Printf("--- DRY RUN: would release reservation %q ---\n", jobID)
		found := false
		for _, s := range summaries {
			if s.JobID == jobID {
				found = true
				fmt.Printf("  Would restore: %s\n", strings.Join(s.EvictedServices, ", "))
			}
		}
		if !found {
			fmt.Println("  No active reservation with this job id (release would be a no-op).")
		}
		fmt.Println("No changes made.")
		return nil
	}

	jctx := jobs.JobContext{LogFn: func(_ string, msg string) { fmt.Println(msg) }}
	restored, err := handler.Release(jctx, jobID)
	if err != nil {
		return fmt.Errorf("release %q: %w", jobID, err)
	}
	if len(restored) == 0 {
		fmt.Printf("ℹ️  No services were tagged under reservation %q (already released, or nothing was ever evicted).\n", jobID)
		return nil
	}
	fmt.Printf("✅ Restored: %s\n", strings.Join(restored, ", "))
	return nil
}
