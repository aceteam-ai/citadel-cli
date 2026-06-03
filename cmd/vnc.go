// cmd/vnc.go
package cmd

import (
	"fmt"
	"os"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/spf13/cobra"
)

var (
	vncPassword string
	vncPort     int
)

var vncCmd = &cobra.Command{
	Use:   "vnc",
	Short: "Manage VNC server on this node",
	Long: `Install, configure, and manage a VNC server for remote desktop access.

On Windows, this installs and configures TightVNC as a system service.
On Linux and macOS, VNC provisioning is currently a stub (most nodes are headless).`,
}

var vncEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Install and start VNC server",
	Long: `Installs a VNC server if not already present, configures the password
and port, and starts the service. This command is idempotent.

If no password is specified, a random 8-character password is generated
and printed to stdout.`,
	Example: `  # Enable with auto-generated password
  citadel vnc enable

  # Enable with specific password and port
  citadel vnc enable --password mypass --port 5900`,
	RunE: runVNCEnable,
}

var vncDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Stop VNC server",
	Long:  `Stops the VNC server service. Does not uninstall it.`,
	RunE:  runVNCDisable,
}

var vncStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show VNC server status",
	Long:  `Displays whether a VNC server is installed, running, and on which port.`,
	RunE:  runVNCStatus,
}

func init() {
	rootCmd.AddCommand(vncCmd)
	vncCmd.AddCommand(vncEnableCmd)
	vncCmd.AddCommand(vncDisableCmd)
	vncCmd.AddCommand(vncStatusCmd)

	vncEnableCmd.Flags().StringVar(&vncPassword, "password", "", "VNC password (auto-generated if not specified)")
	vncEnableCmd.Flags().IntVar(&vncPort, "port", platform.DefaultVNCPort, "VNC port number")
}

func runVNCEnable(cmd *cobra.Command, args []string) error {
	mgr := platform.GetVNCManager()

	// Generate password if not provided
	password := vncPassword
	if password == "" {
		pw, err := platform.GenerateVNCPassword()
		if err != nil {
			return fmt.Errorf("failed to generate password: %w", err)
		}
		password = pw
	}

	// Truncate to 8 chars (VNC DES limit)
	if len(password) > 8 {
		password = password[:8]
		fmt.Println("Note: VNC password truncated to 8 characters (protocol limit)")
	}

	// Validate port
	if err := platform.ValidateVNCPort(vncPort); err != nil {
		return err
	}

	// Install if needed
	if !mgr.IsInstalled() {
		fmt.Println("VNC server not found, installing...")
		if err := mgr.Install(); err != nil {
			return fmt.Errorf("failed to install VNC server: %w", err)
		}
	} else {
		fmt.Println("VNC server already installed.")
	}

	// Configure
	fmt.Printf("Configuring VNC on port %d...\n", vncPort)
	if err := mgr.Configure(password, vncPort); err != nil {
		return fmt.Errorf("failed to configure VNC server: %w", err)
	}

	// Start
	if !mgr.IsRunning() {
		fmt.Println("Starting VNC server...")
		if err := mgr.Start(); err != nil {
			return fmt.Errorf("failed to start VNC server: %w", err)
		}
	}

	fmt.Println()
	fmt.Println("VNC server enabled.")
	fmt.Printf("  Port:     %d\n", vncPort)
	if vncPassword == "" {
		// Only print generated password (user-provided passwords should not be echoed)
		fmt.Printf("  Password: %s\n", password)
	}

	return nil
}

func runVNCDisable(cmd *cobra.Command, args []string) error {
	mgr := platform.GetVNCManager()

	if !mgr.IsRunning() {
		fmt.Println("VNC server is not running.")
		return nil
	}

	fmt.Println("Stopping VNC server...")
	if err := mgr.Stop(); err != nil {
		return fmt.Errorf("failed to stop VNC server: %w", err)
	}

	fmt.Println("VNC server stopped.")
	return nil
}

func runVNCStatus(cmd *cobra.Command, args []string) error {
	mgr := platform.GetVNCManager()

	installed := mgr.IsInstalled()
	running := mgr.IsRunning()
	port := mgr.Port()

	fmt.Printf("Installed: %v\n", installed)
	fmt.Printf("Running:   %v\n", running)
	if running && port > 0 {
		fmt.Printf("Port:      %d\n", port)
	}

	if !installed {
		fmt.Println("\nRun 'citadel vnc enable' to install and start the VNC server.")
		os.Exit(0)
	}

	if !running {
		fmt.Println("\nVNC server is installed but not running.")
		fmt.Println("Run 'citadel vnc enable' to start it.")
	}

	return nil
}
