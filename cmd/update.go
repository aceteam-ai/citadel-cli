// cmd/update.go
package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

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

var updateInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Download and install the latest version",
	Long: `Downloads the latest version from GitHub, verifies the checksum,
backs up the current version, and installs the new binary.

If the new version fails to start, it will automatically roll back
to the previous version.`,
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
	Short: "Enable automatic update checks",
	Long:  `Enables periodic update checks when running citadel work.`,
	Run: func(cmd *cobra.Command, args []string) {
		setAutoUpdate(true)
	},
}

var updateDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable automatic update checks",
	Long:  `Disables automatic update checks.`,
	Run: func(cmd *cobra.Command, args []string) {
		setAutoUpdate(false)
	},
}

func init() {
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
	fmt.Println("\nRun 'citadel version' to verify.")
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
		fmt.Println("Auto-update checks enabled.")
		fmt.Println("Updates will be checked once per day when running 'citadel work'.")
	} else {
		fmt.Println("Auto-update checks disabled.")
		fmt.Println("Run 'citadel update check' to manually check for updates.")
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
