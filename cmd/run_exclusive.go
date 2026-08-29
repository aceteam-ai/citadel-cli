// cmd/run_exclusive.go
//
// `citadel run <service> --exclusive` (aceteam#8248) reserves the GPU for one
// service: evict every other non-pinned service, start the target, and
// restore the evicted peers when the reservation is released. Design:
// docs/design-model-exclusivity.md. It is a variant of SERVICE_START's
// eviction behavior (internal/jobs.ServiceHandler.Reserve/ReserveExclusive,
// #832/this design), layered on top of a normal `citadel run <service>` --
// NOT a variant of run's own add-and-start logic, which is why this drives
// internal/jobs.ServiceHandler directly instead of cmd/service.go's
// startService.
//
// Ownership/crash-safety shape is §2.3(a): this CLI process calls
// Reserve/ReserveExclusive/Release directly (no worklock, no job dispatch
// into a running worker) -- see internal/jobs/model_exclusivity.go's package
// doc comment for the exact contract and the RACE this shape does NOT close
// (a `citadel work` booting mid-exclusive-run can conclude the reservation is
// orphaned and restore the evicted peers out from under it). That risk is
// real, not merely theoretical, and is called out in this command's --help
// text below rather than left implicit.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/spf13/cobra"
)

var (
	exclusiveRun    bool
	exclusiveVRAMGB float64
)

func init() {
	runCmd.Flags().BoolVar(&exclusiveRun, "exclusive", false,
		"Reserve the GPU exclusively for this service: durably evict every other non-pinned "+
			"service first, then start this one. Restores evicted peers on Ctrl-C (or immediately, "+
			"under --detach=true, once the service is up -- see --detach). See 'citadel module "+
			"reservations' to inspect or manually release a stuck reservation.")
	runCmd.Flags().Float64Var(&exclusiveVRAMGB, "vram", 0,
		"With --exclusive, reserve exactly this many GB (an ordinary, satisfiable Reserve) instead "+
			"of the default of evicting every non-pinned service unconditionally.")
}

// runServiceExclusive is dispatched from runCmd.Run when --exclusive is set.
func runServiceExclusive(serviceName string) {
	if err := refuseIfReservationNodeDirUnsupported("citadel run --exclusive"); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	manifest, configDir, err := findOrCreateManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to initialize configuration: %v\n", err)
		os.Exit(1)
	}
	if !serviceIsKnown(serviceName, manifest) {
		fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
		fmt.Printf("Available services: %s\n", strings.Join(knownServiceNames(manifest), ", "))
		os.Exit(1)
	}

	handler := jobs.NewServiceHandlerWithWorkspace(configDir, resolveWorkspaceDir())
	jctx := jobs.JobContext{LogFn: func(_ string, msg string) { fmt.Println(msg) }}
	jobID := jobs.ExclusiveReservationJobID(serviceName)

	fmt.Printf("--- 🔒 Reserving the GPU exclusively for '%s' ---\n", serviceName)
	var res *jobs.Reservation
	if exclusiveVRAMGB > 0 {
		budget := uint64(exclusiveVRAMGB * 1024 * 1024 * 1024)
		res, err = handler.Reserve(jctx, jobID, budget)
	} else {
		res, err = handler.ReserveExclusive(jctx, jobID, serviceName)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to reserve GPU for '%s': %v\n", serviceName, err)
		if res != nil && len(res.Evicted) > 0 {
			fmt.Fprintf(os.Stderr, "   Some services were already evicted before the failure: %s\n", strings.Join(res.Evicted, ", "))
			fmt.Fprintf(os.Stderr, "   Run 'citadel module reservations release %s' to restore them.\n", jobID)
		}
		os.Exit(1)
	}
	if len(res.Evicted) > 0 {
		fmt.Printf("   - Evicted to free VRAM: %s\n", strings.Join(res.Evicted, ", "))
		fmt.Println("     (a service already durably stopped before this reservation still appears here if it was a")
		fmt.Println("     preemption candidate -- it was not necessarily RUNNING; see docs/design-model-exclusivity.md §2.2)")
	} else {
		fmt.Println("   - Nothing needed to be evicted.")
	}
	fmt.Printf("   - %s\n", res.Reason)

	// An explicit exclusive run clears the durable stopped marker (mirrors
	// runSingleService's identical behavior for a plain `citadel run`) so the
	// service also starts on the next boot.
	if err := setServiceDesiredStatus(configDir, serviceName, ""); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not clear stopped marker for %s: %v\n", serviceName, err)
	}

	fmt.Printf("--- 🚀 Starting '%s' ---\n", serviceName)
	if _, startErr := handler.StartServiceWithModel(jctx, serviceName, "", 0); startErr != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to start '%s': %v\n", serviceName, startErr)
		fmt.Fprintln(os.Stderr, "   The GPU reservation is still held (evicted peers are NOT restored automatically on a")
		fmt.Fprintf(os.Stderr, "   failed start). Run 'citadel module reservations release %s' to restore them, or fix the\n", jobID)
		fmt.Fprintln(os.Stderr, "   problem and retry.")
		os.Exit(1)
	}
	fmt.Printf("✅ '%s' is running with exclusive GPU access.\n", serviceName)
	fmt.Printf("   Reservation ID: %s\n", jobID)

	if detachRun {
		fmt.Println("\n(--detach, the default: the reservation stays held in the background.")
		fmt.Printf(" Run 'citadel module reservations release %s' when you're done to restore evicted peers.)\n", jobID)
		return
	}

	fmt.Println("\nPress Ctrl+C to release the reservation and restore evicted services.")
	waitForExclusiveReleaseSignal(handler, jctx, jobID)
}

// waitForExclusiveReleaseSignal blocks until an interrupt/termination signal
// (including SIGHUP, so a dropped SSH session releases the reservation rather
// than orphaning it) and then releases jobID, restoring whatever it evicted.
func waitForExclusiveReleaseSignal(handler *jobs.ServiceHandler, jctx jobs.JobContext, jobID string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	<-ctx.Done()

	fmt.Println("\n--- 🔓 Releasing GPU reservation ---")
	restored, err := handler.Release(jctx, jobID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Some services could not be restored: %v\n", err)
		fmt.Fprintf(os.Stderr, "   Run 'citadel module reservations release %s' to retry, or 'citadel module reservations list' to inspect.\n", jobID)
		os.Exit(1)
	}
	if len(restored) > 0 {
		fmt.Printf("✅ Restored: %s\n", strings.Join(restored, ", "))
	} else {
		fmt.Println("✅ Nothing to restore.")
	}
}

// runCmdDispatchExclusive is called from runCmd.Run before any other
// dispatch when --exclusive is set. Kept as a small separate function (not
// inlined into the Run closure) so the "exactly one service name" validation
// has one obvious place to read.
func runCmdDispatchExclusive(cmd *cobra.Command, args []string) bool {
	if !exclusiveRun {
		return false
	}
	if restartServices {
		fmt.Fprintln(os.Stderr, "❌ --exclusive cannot be combined with --restart.")
		os.Exit(1)
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "❌ --exclusive requires exactly one service name: citadel run <service> --exclusive")
		os.Exit(1)
	}
	runServiceExclusive(args[0])
	return true
}
