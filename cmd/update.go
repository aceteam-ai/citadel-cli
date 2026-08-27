// cmd/update.go
package cmd

import (
	"fmt"
	"io"
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
		"Restart the managed citadel service (if any) after installing, so the new binary actually runs. "+
			"Note: this is an abrupt restart with no drain -- in-flight jobs are dropped, unlike the automatic "+
			"AGENT_UPDATE/auto-updater paths, which drain and wait for idle first.")

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
// I/O, with the target resolver and output streams injected so it is
// unit-testable without touching systemctl/launchctl/sc or os.Stdout/Stderr.
// Returns the process exit code the caller should use (0 = no exit needed).
func runManagedServiceGate(doRestart bool, resolve func() (managedServiceTarget, bool), out, errOut io.Writer) int {
	target, found := resolve()
	if !found {
		return 0
	}

	if !doRestart {
		fmt.Fprintln(out)
		fmt.Fprintln(out, formatManagedServiceWarning(target))
		return 0
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

// warnOrRestartManagedService is the manual-CLI-path counterpart to the
// AGENT_UPDATE job handler's self-restart (internal/worker/agent_update.go)
// and the AutoUpdater (internal/update/autoupdater.go) -- both of those
// already drain, wait for idle, and syscall.Exec-restart on their own. This
// closes the one remaining gap: `citadel update install` run by hand
// (citadel#454). doRestart mirrors updateInstallRestart's default-false,
// opt-in-only contract; it is a parameter (not a direct flag read) so this
// function is unit-testable.
func warnOrRestartManagedService(doRestart bool) {
	code := runManagedServiceGate(doRestart, resolveManagedServiceRestartTarget, os.Stdout, os.Stderr)
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
