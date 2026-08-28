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
	svcForce      bool
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
		cmd.Flags().BoolVar(&svcForce, "force", false, "Install even if a competing citadel managed service is already active (adopts/overwrites; does not stop the existing unit)")
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

// activeManagedUnitFn and newServiceManagerFn are indirections over
// service.ActiveManagedUnit / service.NewManager so runSvcInstall's
// competing-unit refusal is testable end-to-end without shelling out to
// systemctl or writing real unit files (which mgr.Install does for real,
// including a daemon-reload and enable). Tests swap these and restore them.
var (
	activeManagedUnitFn = service.ActiveManagedUnit
	newServiceManagerFn = service.NewManager
)

func runSvcInstall(_ *cobra.Command, _ []string) error {
	cfg, err := service.DefaultConfig()
	if err != nil {
		return err
	}
	cfg.UserMode = resolveUserMode()

	if !svcForce {
		if unit, competing := competingManagedUnit(cfg, activeManagedUnitFn); competing {
			return competingManagedUnitError(unit)
		}
	}

	mgr := newServiceManagerFn()
	return mgr.Install(cfg)
}

// competingManagedUnit reports whether an already-ACTIVE citadel-managed
// systemd unit (detect scans both unit families it recognizes -- see
// service.ActiveManagedUnit's own doc comment) refers to something other than
// the unit `citadel service install` is about to write for cfg.
//
// "Matches" requires both the unit name (service.ServiceName, "citadel" --
// the fleet's citadel-worker.service can therefore never match) AND the
// user/system scope: a same-named unit in the OTHER scope still lands at a
// different systemd unit path (~/.config/systemd/user/... vs
// /etc/systemd/system/...) and would run ALONGSIDE the existing one, not
// replace it -- exactly the duplicate-unit outcome citadel#882 is about.
//
// No active unit, or an active unit that already IS the one about to be
// installed, is not competing: the former has nothing to collide with, the
// latter makes `citadel service install` an idempotent re-install.
//
// detect is injected (service.ActiveManagedUnit in production) so this stays
// unit-testable without shelling out to systemctl, mirroring
// resolveManagedServiceRestartTarget / managedServiceTargetFromManagerStatus
// in cmd/update.go.
func competingManagedUnit(cfg service.ServiceConfig, detect func() (service.ManagedUnit, bool)) (service.ManagedUnit, bool) {
	unit, found := detect()
	if !found {
		return service.ManagedUnit{}, false
	}
	if unit.Name == service.ServiceName && unit.UserMode == cfg.UserMode {
		return service.ManagedUnit{}, false
	}
	return unit, true
}

// competingManagedUnitError renders the refusal for an install blocked by
// competingManagedUnit. It names the existing unit and gives an exact stop
// command (mirroring the format ManagedUnit.RestartCommand already uses) plus
// the --force escape hatch.
func competingManagedUnitError(unit service.ManagedUnit) error {
	stopCmd := fmt.Sprintf("sudo systemctl stop %s", unit.Name)
	if unit.UserMode {
		stopCmd = fmt.Sprintf("systemctl --user stop %s", unit.Name)
	}
	return fmt.Errorf(
		"refusing to install: a competing citadel managed service is already active (%s)\n"+
			"Installing another would create a duplicate unit running citadel twice.\n\n"+
			"  Stop/disable it first:  %s\n"+
			"  Or install anyway:      citadel service install --force\n"+
			"                          (this does NOT stop the existing unit; you will end up\n"+
			"                          with two managed citadel services running)",
		unit.Description(), stopCmd)
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
