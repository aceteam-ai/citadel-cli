// cmd/update.go
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/service"
	"github.com/aceteam-ai/citadel-cli/internal/tui/whimsy"
	"github.com/aceteam-ai/citadel-cli/internal/update"
	"github.com/spf13/cobra"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Manage Citadel CLI updates",
	Long: `Check for, install, and manage Citadel CLI updates.

The auto-update feature periodically checks for new versions and can
install them with a single command. A previous version is always kept
for rollback if needed.

Examples:
  citadel update check      # Check for available updates
  citadel update install    # Download and install the latest version
  citadel update status     # Show update status and versions
  citadel update rollback   # Restore the previous version
  citadel update enable     # Enable auto-update checks
  citadel update disable    # Disable auto-update checks`,
	Run: func(cmd *cobra.Command, args []string) {
		// Default behavior: show status
		showUpdateStatus()
	},
}

var updateCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check for available updates",
	Long:  `Checks GitHub releases for a newer version of Citadel CLI.`,
	Run: func(cmd *cobra.Command, args []string) {
		checkForUpdate()
	},
}

// updateInstallRestart is the --restart flag on `citadel update install`. It
// is opt-in and defaults to false: an interactive CLI invocation must never
// restart a managed service out from under an operator without being asked
// to (see the warn-vs-restart split in installUpdate).
var updateInstallRestart bool

var updateInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Download and install the latest version",
	Long: `Downloads the latest version from GitHub, verifies the checksum,
backs up the current version, and installs the new binary.

If the new version fails to start, it will automatically roll back
to the previous version.

If citadel is running as a managed service (systemd/launchd/Windows
service), swapping the binary on disk does NOT restart the already-running
process -- it keeps executing the old code until something restarts it
(citadel#454). This command detects that and warns loudly by default; pass
--restart to have it restart the managed service for you.`,
	Run: func(cmd *cobra.Command, args []string) {
		installUpdate()
	},
}

var updateRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Restore the previous version",
	Long:  `Restores the previously installed version of Citadel CLI.`,
	Run: func(cmd *cobra.Command, args []string) {
		rollbackUpdate()
	},
}

var updateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show update status and version information",
	Long:  `Displays the current version, previous version, and update settings.`,
	Run: func(cmd *cobra.Command, args []string) {
		showUpdateStatus()
	},
}

var updateEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable automatic updates",
	Long: `Enables automatic updates. A running 'citadel work' agent periodically
checks for a newer release and installs it (draining in-flight jobs first).
The setting is persisted and re-read each cycle, so it takes effect on a
running agent without a restart.`,
	Run: func(cmd *cobra.Command, args []string) {
		setAutoUpdate(true)
	},
}

var updateDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable automatic updates",
	Long: `Disables automatic updates. Takes effect on a running 'citadel work'
agent within one check interval, without a restart.`,
	Run: func(cmd *cobra.Command, args []string) {
		setAutoUpdate(false)
	},
}

func init() {
	updateInstallCmd.Flags().BoolVar(&updateInstallRestart, "restart", false,
		fmt.Sprintf("Restart the managed citadel service (if any) after installing, so the new binary actually runs. "+
			"Before restarting, waits up to %s for the running worker's in-flight jobs to finish (best-effort, "+
			"via GET /worker; citadel#887) -- restarts anyway once that elapses. If /worker is unreachable "+
			"(an older worker predating the route, or no status listener at all) the wait is skipped entirely "+
			"and this restarts immediately, same as before #887. Unlike the automatic AGENT_UPDATE/auto-updater "+
			"paths, this is not a hard drain: it does not stop new jobs from being picked up while waiting.",
			managedServiceDrainTimeout))

	rootCmd.AddCommand(updateCmd)
	updateCmd.AddCommand(updateCheckCmd)
	updateCmd.AddCommand(updateInstallCmd)
	updateCmd.AddCommand(updateRollbackCmd)
	updateCmd.AddCommand(updateStatusCmd)
	updateCmd.AddCommand(updateEnableCmd)
	updateCmd.AddCommand(updateDisableCmd)
}

func checkForUpdate() {
	spinner := whimsy.NewSimpleSpinner(whimsy.ProcessingMessages)
	spinner.Start()

	client := update.NewClient(Version)
	release, err := client.CheckForUpdate()
	if err != nil {
		spinner.StopWithError(fmt.Sprintf("Error checking for updates: %v", err))
		os.Exit(1)
	}

	// Update last check time
	state, _ := update.LoadState()
	update.UpdateLastCheck(state)
	_ = update.SaveState(state)

	if release == nil {
		spinner.StopWithSuccess(fmt.Sprintf("You are running the latest version (%s)", Version))
		return
	}

	spinner.Stop()
	fmt.Printf("\nUpdate available: %s -> %s\n", Version, release.TagName)
	fmt.Printf("Release: %s\n", release.Name)
	fmt.Printf("URL: %s\n", release.HTMLURL)
	fmt.Println("\nRun 'citadel update install' to update.")
}

func installUpdate() {
	// citadel#926: self-heal before attempting a new swap, in case a prior
	// update was interrupted mid-swap (also checked in PersistentPreRun, but
	// checked again here so `citadel update install` is a reliable manual
	// recovery action on its own).
	if recovered, err := update.RecoverInterruptedSwap(); err != nil {
		fmt.Printf("Warning: interrupted update recovery check failed: %v\n", err)
	} else if recovered {
		fmt.Println("Recovered from an interrupted update before installing.")
	}

	// Check for updates
	checkSpinner := whimsy.NewSimpleSpinner(whimsy.ProcessingMessages)
	checkSpinner.Start()

	client := update.NewClient(Version)
	release, err := client.CheckForUpdate()
	if err != nil {
		checkSpinner.StopWithError(fmt.Sprintf("Error checking for updates: %v", err))
		os.Exit(1)
	}

	if release == nil {
		checkSpinner.StopWithSuccess(fmt.Sprintf("You are running the latest version (%s)", Version))
		return
	}

	checkSpinner.StopWithSuccess(fmt.Sprintf("Update available: %s -> %s", Version, release.TagName))

	// Download update
	dlSpinner := whimsy.NewSimpleSpinner(whimsy.DownloadMessages)
	dlSpinner.Start()

	pendingPath := update.GetPendingBinaryPath()
	if err := client.DownloadAndVerify(release, pendingPath); err != nil {
		dlSpinner.StopWithError(fmt.Sprintf("Error downloading update: %v", err))
		os.Exit(1)
	}

	dlSpinner.StopWithSuccess("Downloaded and verified checksum")

	// Install update
	installSpinner := whimsy.NewSimpleSpinner(whimsy.ProvisioningMessages)
	installSpinner.Start()

	if err := update.ApplyUpdate(pendingPath); err != nil {
		installSpinner.StopWithError(fmt.Sprintf("Error installing update: %v", err))
		os.Exit(1)
	}

	// Update state
	state, _ := update.LoadState()
	update.RecordUpdate(state, Version, release.TagName)
	update.UpdateLastCheck(state)
	_ = update.SaveState(state)

	installSpinner.StopWithSuccess(fmt.Sprintf("Successfully updated to %s", release.TagName))
	fmt.Println("Previous version saved for rollback.")

	// Re-materialize managed systemd unit files so template/hardening changes in
	// the new binary (e.g. the #444 crash-restart-storm hardening) actually reach
	// this already-deployed node. The binary swap above replaces only the binary;
	// the on-disk unit was written once at install time and is otherwise never
	// refreshed on a version change (#426 does the same for compose files).
	// Idempotent: a unit already carrying the hardening is left untouched.
	if rewritten, err := service.RematerializeManagedUnits(func(format string, args ...any) {
		fmt.Printf("   - "+format+"\n", args...)
	}); err != nil {
		fmt.Printf("Warning: could not refresh managed service unit(s): %v\n", err)
	} else if len(rewritten) > 0 {
		fmt.Printf("Refreshed managed service unit(s): %s\n", strings.Join(rewritten, ", "))
		fmt.Println("The new restart policy applies on the next service restart.")
	}

	fmt.Println("\nRun 'citadel version' to verify.")

	// The binary on disk is now the new version, but a managed service's
	// already-running process has NOT been restarted -- it is still executing
	// the old code (citadel#454). RematerializeManagedUnits above deliberately
	// never restarts anything ("the update flow handles restart on its own
	// terms"); this is that "own terms" for the manual CLI path: warn loudly by
	// default, or restart when the operator explicitly opted in via --restart.
	warnOrRestartManagedService(updateInstallRestart)
}

func rollbackUpdate() {
	// citadel#926: self-heal before attempting a rollback swap, for the same
	// reason as installUpdate above.
	if recovered, err := update.RecoverInterruptedSwap(); err != nil {
		fmt.Printf("Warning: interrupted update recovery check failed: %v\n", err)
	} else if recovered {
		fmt.Println("Recovered from an interrupted update before rolling back.")
	}

	if !update.HasPreviousVersion() {
		fmt.Fprintln(os.Stderr, "No previous version available for rollback.")
		os.Exit(1)
	}

	// Show what we're rolling back to
	prevInfo, _ := update.GetPreviousVersionInfo()
	if prevInfo != "" {
		fmt.Printf("Rolling back to: %s", strings.TrimSpace(prevInfo))
	} else {
		fmt.Println("Rolling back to previous version...")
	}

	if err := update.Rollback(); err != nil {
		fmt.Fprintf(os.Stderr, "Error rolling back: %v\n", err)
		os.Exit(1)
	}

	// Update state
	state, _ := update.LoadState()
	if state.PreviousVersion != "" {
		state.CurrentVersion, state.PreviousVersion = state.PreviousVersion, state.CurrentVersion
		_ = update.SaveState(state)
	}

	fmt.Println("\nRollback complete.")
	fmt.Println("Run 'citadel version' to verify.")
}

func showUpdateStatus() {
	state, err := update.LoadState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading update state: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Citadel CLI Update Status")
	fmt.Println("-------------------------")
	fmt.Printf("Current version:  %s\n", Version)

	if state.PreviousVersion != "" {
		fmt.Printf("Previous version: %s\n", state.PreviousVersion)
	} else {
		fmt.Printf("Previous version: (none)\n")
	}

	if !state.LastCheck.IsZero() {
		fmt.Printf("Last check:       %s\n", state.LastCheck.Format(time.RFC3339))
	} else {
		fmt.Printf("Last check:       (never)\n")
	}

	if !state.LastUpdate.IsZero() {
		fmt.Printf("Last update:      %s\n", state.LastUpdate.Format(time.RFC3339))
	}

	fmt.Printf("Auto-update:      %v\n", state.AutoUpdate)
	fmt.Printf("Channel:          %s\n", state.Channel)

	// Check for available update
	fmt.Println("\nChecking for updates...")
	client := update.NewClient(Version)
	release, err := client.CheckForUpdate()
	if err != nil {
		fmt.Printf("Update check:     failed (%v)\n", err)
	} else if release == nil {
		fmt.Println("Update check:     up to date")
	} else {
		fmt.Printf("Update available: %s\n", release.TagName)
		fmt.Println("\nRun 'citadel update install' to update.")
	}
}

func setAutoUpdate(enabled bool) {
	state, err := update.LoadState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading update state: %v\n", err)
		os.Exit(1)
	}

	state.AutoUpdate = enabled
	if err := update.SaveState(state); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving update state: %v\n", err)
		os.Exit(1)
	}

	if enabled {
		fmt.Println("Automatic updates enabled.")
		fmt.Println("A running 'citadel work' agent will periodically check for and install newer releases.")
	} else {
		fmt.Println("Automatic updates disabled.")
		fmt.Println("Run 'citadel update check' / 'citadel update install' to update manually.")
	}
}

// managedServiceTarget describes a detected managed citadel service whose
// running process has not yet picked up a just-installed binary, plus how to
// restart it.
type managedServiceTarget struct {
	Description string
	RestartCmd  string
	Restart     func() error
}

// resolveManagedServiceRestartTarget decides whether citadel is currently
// running as a managed service that needs restarting to load a just-installed
// binary, and if so, how. It layers two signals:
//
//  1. service.ActiveManagedUnit -- a direct systemd-unit scan (Linux only, a
//     no-op stub elsewhere). This is the ONLY signal that sees the
//     install.sh/packer fleet unit (citadel-worker.service), which is how
//     citadel actually ships on most nodes and is a completely different unit
//     from the one service.Manager below manages.
//  2. service.NewManager().Status() -- the cross-platform abstraction behind
//     `citadel service install/start/stop/status`. This is the only signal on
//     macOS (launchd) and Windows (SCM), and also covers a Linux node set up
//     via `citadel service install` rather than install.sh.
//
// Deliberately NOT used: this process's own environment (CITADEL_SERVICE,
// INVOCATION_ID -- see managedByServiceManager in cmd/agent_tools.go). Those
// answer "is *this* process running under a service manager", which is the
// right question for the AGENT_UPDATE handler (it runs inside the worker it
// restarts) but the wrong one here: `citadel update install` run manually
// (e.g. an operator's plain SSH shell) is a different, unrelated process from
// the managed worker, so its own env says nothing about whether a managed
// worker exists on this host -- checking it would make the detection silently
// never fire in exactly the scenario citadel#454 reported.
func resolveManagedServiceRestartTarget() (managedServiceTarget, bool) {
	if unit, ok := service.ActiveManagedUnit(); ok {
		return managedServiceTarget{
			Description: unit.Description(),
			RestartCmd:  unit.RestartCommand(),
			Restart:     unit.Restart,
		}, true
	}

	mgr := service.NewManager()
	st, err := mgr.Status()
	if err != nil {
		return managedServiceTarget{}, false
	}
	return managedServiceTargetFromManagerStatus(st, mgr)
}

// managedServiceTargetFromManagerStatus is the pure decision given a
// service.ServiceStatus, split out from resolveManagedServiceRestartTarget so
// it can be unit-tested without shelling out to systemctl/launchctl/sc. A
// service that is installed but not currently running has no live process to
// be split-brained with, so it is not a restart target.
func managedServiceTargetFromManagerStatus(st *service.ServiceStatus, mgr service.Manager) (managedServiceTarget, bool) {
	if st == nil || !st.Installed || !st.Running {
		return managedServiceTarget{}, false
	}
	return managedServiceTarget{
		Description: "citadel service",
		// service.Manager doesn't expose a single Restart(); the CLI's own
		// stop+start subcommands are the existing, exact equivalent and are
		// valid on every platform this Manager backs.
		RestartCmd: "citadel service stop && citadel service start",
		Restart: func() error {
			if err := mgr.Stop(); err != nil {
				return err
			}
			return mgr.Start()
		},
	}, true
}

// formatManagedServiceWarning renders the loud, hard-to-miss warning printed
// when a managed service was detected but --restart was not passed.
func formatManagedServiceWarning(target managedServiceTarget) string {
	var b strings.Builder
	sep := strings.Repeat("=", 70)
	fmt.Fprintln(&b, sep)
	fmt.Fprintf(&b, "WARNING: citadel is running as a managed service (%s).\n", target.Description)
	fmt.Fprintln(&b, "The binary on disk was updated, but the RUNNING process was NOT")
	fmt.Fprintln(&b, "restarted -- it is still executing the OLD code until you restart it.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "  Restart now:  %s\n", target.RestartCmd)
	fmt.Fprintln(&b, "  Or next time: citadel update install --restart")
	fmt.Fprint(&b, sep)
	return b.String()
}

// runManagedServiceGate implements the warn-vs-restart decision and all of its
// I/O, with the target resolver, drain step, and output streams injected so it
// is unit-testable without touching systemctl/launchctl/sc, os.Stdout/Stderr,
// or a live worker's /worker endpoint.
//
// drain runs only on the doRestart==true, found==true path, immediately before
// target.Restart() -- see drainManagedServiceBeforeRestart for what it does
// (citadel#887's best-effort in-flight wait). A nil drain skips the wait
// entirely (used by callers/tests that don't exercise it), which is
// byte-identical to the pre-#887 immediate-restart behavior.
func runManagedServiceGate(doRestart bool, resolve func() (managedServiceTarget, bool), drain func(out io.Writer), out, errOut io.Writer) int {
	target, found := resolve()
	if !found {
		return 0
	}

	if !doRestart {
		fmt.Fprintln(out)
		fmt.Fprintln(out, formatManagedServiceWarning(target))
		return 0
	}

	if drain != nil {
		drain(out)
	}

	fmt.Fprintf(out, "\nRestarting managed service (%s) to load the new binary...\n", target.Description)
	if err := target.Restart(); err != nil {
		fmt.Fprintf(errOut, "Error restarting service: %v\n", err)
		fmt.Fprintf(out, "Restart it manually: %s\n", target.RestartCmd)
		return 1
	}
	fmt.Fprintln(out, "Service restarted; the new binary is now running.")
	return 0
}

// managedServiceDrainTimeout bounds how long drainManagedServiceBeforeRestart
// will wait for the running worker's in-flight job count to reach zero before
// giving up and restarting anyway. Package var so tests can shrink it rather
// than sleep for real.
var managedServiceDrainTimeout = 30 * time.Second

// managedServiceDrainPollInterval is how often drainManagedServiceBeforeRestart
// re-polls /worker while waiting.
var managedServiceDrainPollInterval = 2 * time.Second

// workerInFlightFetcher fetches the current in-flight job count from the
// running worker. ok=false means "could not determine" -- unreachable (no
// status listener, connection refused), a 404 (an older worker predating the
// /worker route), or any other probe failure -- and callers must treat that
// as "skip the wait", never as "in_flight is 0".
type workerInFlightFetcher func() (inFlight int64, ok bool)

// fetchWorkerInFlight GETs baseURL+"/worker" and extracts worker.in_flight.
// Split out from defaultWorkerInFlightFetcher (which resolves baseURL via
// resolveStatusPort()) so it is testable against an httptest.Server directly,
// without needing to fake gateway-facts.json resolution.
//
// It reuses the same route/shape probeWorkerPubSubTransport (cmd/status.go)
// already reads (citadel#735): GET /worker serves an in-memory
// WorkerLiveness snapshot and shells out to nothing, so this is cheap enough
// to poll on a short interval without adding load to a busy node.
func fetchWorkerInFlight(baseURL string) (int64, bool) {
	body, err := httpGetBodyErr(&http.Client{Timeout: pubSubProbeCheapTimeout}, baseURL+"/worker")
	if err != nil {
		// Covers: connection refused (no status listener -- e.g. --status-port 0),
		// a 404 (an older worker predating the /worker route), a timeout, and any
		// other non-2xx. None of these distinguish "definitely zero in-flight" from
		// "we simply could not ask" -- the caller must degrade, not block.
		return 0, false
	}
	var payload struct {
		Worker *struct {
			InFlight int64 `json:"in_flight"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Worker == nil {
		return 0, false
	}
	return payload.Worker.InFlight, true
}

// defaultWorkerInFlightFetcher is the production workerInFlightFetcher: it
// resolves the local status server's port the same way probeWorkerPubSubTransport
// does (resolveStatusPort, cmd/work_attach.go) and reads /worker over loopback.
func defaultWorkerInFlightFetcher() (int64, bool) {
	port := resolveStatusPort()
	if port <= 0 {
		return 0, false
	}
	return fetchWorkerInFlight(fmt.Sprintf("http://127.0.0.1:%d", port))
}

// drainManagedServiceBeforeRestart is the best-effort wait for the managed
// worker's in-flight job count to reach zero before an operator's --restart
// cuts it off (citadel#887, a follow-up to citadel#454/#886).
//
// GET /worker (citadel#735) is what makes this tractable at all from a
// separate, short-lived CLI process: it serves the running worker's own
// WorkerState.InFlight from an in-memory snapshot, so `citadel update install
// --restart` can now observe roughly what the AGENT_UPDATE/auto-updater paths
// already drain for internally. It is still NOT a hard gate, on purpose:
//   - fetch returning ok=false (no status listener, an older worker with no
//     /worker route, a timeout, ...) is NOT evidence of zero in-flight jobs --
//     it is "could not determine", so this returns immediately and lets the
//     restart proceed exactly as it did before #887. Blocking on an unreadable
//     signal would be worse than the abrupt-restart status quo: an operator
//     running --restart on a node with no status server enabled would get a
//     command that hangs for the full timeout every single time, forever, for
//     no benefit.
//   - if in-flight jobs never reach zero within timeout, this restarts anyway
//     (an operator who passed --restart wants the new binary running; refusing
//     forever because a long job never finishes would defeat that ask).
//
// out is written to for progress so the wait is observable without a
// --verbose flag; pass io.Discard to suppress it (there is no nil-writer
// special case, matching every other I/O injection point in this file).
func drainManagedServiceBeforeRestart(fetch workerInFlightFetcher, timeout, pollInterval time.Duration, out io.Writer) {
	deadline := time.Now().Add(timeout)
	for {
		inFlight, ok := fetch()
		if !ok {
			fmt.Fprintln(out, "Could not read the in-flight job count from the running worker "+
				"(no /worker route, or nothing listening on the status port); restarting without waiting.")
			return
		}
		if inFlight == 0 {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(out, "Timed out after %s waiting for %d in-flight job(s) to finish; restarting anyway.\n",
				timeout, inFlight)
			return
		}
		fmt.Fprintf(out, "Waiting for %d in-flight job(s) to finish before restarting (up to %s)...\n",
			inFlight, timeout)
		time.Sleep(pollInterval)
	}
}

// warnOrRestartManagedService is the manual-CLI-path counterpart to the
// AGENT_UPDATE job handler's self-restart (internal/worker/agent_update.go)
// and the AutoUpdater (internal/update/autoupdater.go) -- both of those
// already drain, wait for idle, and syscall.Exec-restart on their own. This
// closes the one remaining gap: `citadel update install` run by hand
// (citadel#454). doRestart mirrors updateInstallRestart's default-false,
// opt-in-only contract; it is a parameter (not a direct flag read) so this
// function is unit-testable.
//
// The drain closure wires defaultWorkerInFlightFetcher into
// drainManagedServiceBeforeRestart (citadel#887) with the package-level
// timeout/poll-interval vars above; runManagedServiceGate only invokes it on
// the doRestart==true, found==true path.
func warnOrRestartManagedService(doRestart bool) {
	drain := func(out io.Writer) {
		drainManagedServiceBeforeRestart(defaultWorkerInFlightFetcher, managedServiceDrainTimeout, managedServiceDrainPollInterval, out)
	}
	code := runManagedServiceGate(doRestart, resolveManagedServiceRestartTarget, drain, os.Stdout, os.Stderr)
	if code != 0 {
		os.Exit(code)
	}
}

// CheckForUpdateInBackground performs a background update check during citadel work
// This is called from work.go and should not block
func CheckForUpdateInBackground() {
	state, err := update.LoadState()
	if err != nil || !update.ShouldCheck(state) {
		return
	}

	client := update.NewClient(Version)
	release, err := client.CheckForUpdate()

	// Update last check time regardless of result
	update.UpdateLastCheck(state)
	_ = update.SaveState(state)

	if err != nil || release == nil {
		return
	}

	// Notify user (don't auto-install)
	fmt.Printf("   - Update available: %s (run 'citadel update install')\n", release.TagName)
}
