// cmd/service_cmd.go
// System service installation and management commands.
//
// Provides "citadel service install/uninstall/start/stop/status" subcommands
// and top-level aliases "citadel install-service", "citadel uninstall-service",
// "citadel service-status" for convenience.
package cmd

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/service"
	"github.com/spf13/cobra"
)

// Flags for service install.
var (
	svcUserMode   bool
	svcSystemMode bool
)

// --- Subcommand group: citadel service ... ---

var svcCmd = &cobra.Command{
	Use:     "service",
	Aliases: []string{"svc"},
	Short:   "Manage Citadel as a system service",
	Long: `Install, uninstall, start, stop, or check the status of Citadel as a system service.

This allows Citadel to run in the background and start automatically on boot.

On Linux:   Creates a systemd unit (user unit by default, --system for system-wide)
On macOS:   Creates a launchd plist (user agent by default, --system for daemon)
On Windows: Creates a Windows Service (always system-wide)`,
}

var svcInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Citadel as a system service",
	Long: `Install Citadel as a managed background service that runs on boot.

By default installs as a user service (no root/admin required on Linux/macOS).
Use --system to install as a system-wide service (requires root/admin).

The service runs "citadel work" with the CITADEL_SERVICE=true environment
variable set, enabling auto-restart on failure and boot-time startup.`,
	RunE: runSvcInstall,
}

var svcUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall the Citadel system service",
	Long: `Stop and remove the Citadel managed service.

This stops the running service, disables auto-start, and removes the service
configuration file. It does NOT remove Citadel config or data.`,
	RunE: runSvcUninstall,
}

var svcStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Citadel service",
	RunE:  runSvcStart,
}

var svcStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Citadel service",
	RunE:  runSvcStop,
}

var svcStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show service installation and running status",
	RunE:  runSvcStatus,
}

// --- Top-level aliases ---

var installServiceCmd = &cobra.Command{
	Use:   "install-service",
	Short: "Install Citadel as a system service (alias for 'service install')",
	Long:  svcInstallCmd.Long,
	RunE:  runSvcInstall,
}

var uninstallServiceCmd = &cobra.Command{
	Use:   "uninstall-service",
	Short: "Uninstall the Citadel system service (alias for 'service uninstall')",
	Long:  svcUninstallCmd.Long,
	RunE:  runSvcUninstall,
}

var serviceStatusCmd = &cobra.Command{
	Use:   "service-status",
	Short: "Show service status (alias for 'service status')",
	RunE:  runSvcStatus,
}

func init() {
	// Subcommand group.
	rootCmd.AddCommand(svcCmd)
	svcCmd.AddCommand(svcInstallCmd)
	svcCmd.AddCommand(svcUninstallCmd)
	svcCmd.AddCommand(svcStartCmd)
	svcCmd.AddCommand(svcStopCmd)
	svcCmd.AddCommand(svcStatusCmd)

	// Top-level aliases.
	rootCmd.AddCommand(installServiceCmd)
	rootCmd.AddCommand(uninstallServiceCmd)
	rootCmd.AddCommand(serviceStatusCmd)

	// Flags for install (shared between subcommand and alias).
	for _, cmd := range []*cobra.Command{svcInstallCmd, installServiceCmd} {
		cmd.Flags().BoolVar(&svcUserMode, "user", false, "Install as a user service (default on Linux/macOS)")
		cmd.Flags().BoolVar(&svcSystemMode, "system", false, "Install as a system-wide service (requires root/admin)")
	}
}

// resolveUserMode determines whether to install as a user or system service.
// --system takes explicit precedence; --user is the default on Linux/macOS.
func resolveUserMode() bool {
	if svcSystemMode {
		return false
	}
	if svcUserMode {
		return true
	}
	// Default: user mode on Linux/macOS, system on Windows.
	return !platform.IsWindows()
}

func runSvcInstall(_ *cobra.Command, _ []string) error {
	cfg, err := service.DefaultConfig()
	if err != nil {
		return err
	}
	cfg.UserMode = resolveUserMode()

	mgr := service.NewManager()
	return mgr.Install(cfg)
}

func runSvcUninstall(_ *cobra.Command, _ []string) error {
	mgr := service.NewManager()
	err := mgr.Uninstall()

	// Clean up VNC if installed.
	vncMgr := platform.GetVNCManager()
	if vncMgr.IsInstalled() {
		fmt.Println("Removing VNC server...")
		if vncErr := vncMgr.Uninstall(); vncErr != nil {
			fmt.Printf("Warning: VNC removal failed: %v\n", vncErr)
		}
	}

	return err
}

func runSvcStart(_ *cobra.Command, _ []string) error {
	mgr := service.NewManager()
	return mgr.Start()
}

func runSvcStop(_ *cobra.Command, _ []string) error {
	mgr := service.NewManager()
	return mgr.Stop()
}

func runSvcStatus(_ *cobra.Command, _ []string) error {
	mgr := service.NewManager()
	st, err := mgr.Status()
	if err != nil {
		return err
	}

	if !st.Installed {
		fmt.Println("Citadel service is not installed.")
		fmt.Println("\nRun 'citadel service install' to install it.")
		return nil
	}

	fmt.Println("Citadel service: installed")
	if st.Running {
		fmt.Printf("  Status:  running (PID %d)\n", st.PID)
	} else {
		fmt.Println("  Status:  stopped")
	}

	if len(st.RecentLogs) > 0 {
		fmt.Println("\nRecent logs:")
		for _, line := range st.RecentLogs {
			fmt.Printf("  %s\n", line)
		}
	}

	return nil
}
