// cmd/service_cmd.go
// System service installation and management commands
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var svcCmd = &cobra.Command{
	Use:     "service",
	Aliases: []string{"svc"},
	Short:   "Manage Citadel as a system service",
	Long: `Install, uninstall, start, stop, or check the status of Citadel as a system service.

This allows Citadel to run in the background and start automatically on boot.`,
}

var svcInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Citadel as a system service",
	Long: `Install Citadel as a system service that runs in the background.

On Linux: Creates a systemd service unit
On macOS: Creates a launchd plist
On Windows: Creates a Windows Service

The service will start automatically on boot and keep your node connected to the AceTeam Network.`,
	RunE: runSvcInstall,
}

var svcUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Uninstall the Citadel system service",
	RunE:  runSvcUninstall,
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
	Short: "Check the status of the Citadel service",
	RunE:  runSvcStatus,
}

func init() {
	rootCmd.AddCommand(svcCmd)
	svcCmd.AddCommand(svcInstallCmd)
	svcCmd.AddCommand(svcUninstallCmd)
	svcCmd.AddCommand(svcStartCmd)
	svcCmd.AddCommand(svcStopCmd)
	svcCmd.AddCommand(svcStatusCmd)
}

// Service file templates
const systemdServiceTemplate = `[Unit]
Description=Citadel - AceTeam Sovereign Compute Agent
Documentation=https://github.com/aceteam-ai/citadel-cli
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
Type=simple
ExecStart=%s --daemon
Restart=always
RestartSec=10
User=%s
Group=%s
Environment=HOME=%s

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=citadel

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`

const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>ai.aceteam.citadel</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>--daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/citadel.log</string>
    <key>StandardErrorPath</key>
    <string>%s/citadel.error.log</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>%s</string>
    </dict>
</dict>
</plist>
`

func runSvcInstall(cmd *cobra.Command, args []string) error {
	switch runtime.GOOS {
	case "linux":
		return installLinuxSvc()
	case "darwin":
		return installDarwinSvc()
	case "windows":
		return installWindowsSvc()
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

func runSvcUninstall(cmd *cobra.Command, args []string) error {
	switch runtime.GOOS {
	case "linux":
		return uninstallLinuxSvc()
	case "darwin":
		return uninstallDarwinSvc()
	case "windows":
		return uninstallWindowsSvc()
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

func runSvcStart(cmd *cobra.Command, args []string) error {
	switch runtime.GOOS {
	case "linux":
		return runSvcCommand("sudo", "systemctl", "start", "citadel")
	case "darwin":
		return runSvcCommand("launchctl", "load", getSvcLaunchdPlistPath())
	case "windows":
		return runSvcCommand("sc", "start", "citadel")
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

func runSvcStop(cmd *cobra.Command, args []string) error {
	switch runtime.GOOS {
	case "linux":
		return runSvcCommand("sudo", "systemctl", "stop", "citadel")
	case "darwin":
		return runSvcCommand("launchctl", "unload", getSvcLaunchdPlistPath())
	case "windows":
		return runSvcCommand("sc", "stop", "citadel")
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

func runSvcStatus(cmd *cobra.Command, args []string) error {
	switch runtime.GOOS {
	case "linux":
		return runSvcCommand("systemctl", "status", "citadel")
	case "darwin":
		return runSvcCommand("launchctl", "list", "ai.aceteam.citadel")
	case "windows":
		return runSvcCommand("sc", "query", "citadel")
	default:
		return fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
}

// Linux (systemd) implementation
func installLinuxSvc() error {
	// Check for root
	if os.Geteuid() != 0 {
		return fmt.Errorf("installing a system service requires root privileges.\nRun: sudo citadel service install")
	}

	// Get the path to the citadel binary
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// Get current user info (the user who ran sudo)
	username := os.Getenv("SUDO_USER")
	if username == "" {
		username = "root"
	}

	homeDir := os.Getenv("HOME")
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		homeDir = "/home/" + sudoUser
		// Check if it's actually there
		if _, err := os.Stat(homeDir); os.IsNotExist(err) {
			homeDir = "/root"
		}
	}

	citadelDir := filepath.Join(homeDir, ".citadel-cli")

	// Create the service file
	serviceContent := fmt.Sprintf(systemdServiceTemplate,
		exePath,
		username,
		username,
		homeDir,
		citadelDir,
	)

	servicePath := "/etc/systemd/system/citadel.service"
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	fmt.Printf("✓ Created systemd service file: %s\n", servicePath)

	// Reload systemd
	if err := runSvcCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}
	fmt.Println("✓ Reloaded systemd daemon")

	// Enable the service
	if err := runSvcCommand("systemctl", "enable", "citadel"); err != nil {
		return fmt.Errorf("failed to enable service: %w", err)
	}
	fmt.Println("✓ Enabled citadel service (will start on boot)")

	// Start the service
	if err := runSvcCommand("systemctl", "start", "citadel"); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}
	fmt.Println("✓ Started citadel service")

	fmt.Println("\nCitadel is now running as a system service!")
	fmt.Println("\nUseful commands:")
	fmt.Println("  sudo systemctl status citadel   - Check status")
	fmt.Println("  sudo systemctl stop citadel     - Stop service")
	fmt.Println("  sudo systemctl restart citadel  - Restart service")
	fmt.Println("  sudo journalctl -u citadel -f   - View logs")

	return nil
}

func uninstallLinuxSvc() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("uninstalling a system service requires root privileges.\nRun: sudo citadel service uninstall")
	}

	// Stop the service if running
	_ = runSvcCommand("systemctl", "stop", "citadel")
	fmt.Println("✓ Stopped citadel service")

	// Disable the service
	_ = runSvcCommand("systemctl", "disable", "citadel")
	fmt.Println("✓ Disabled citadel service")

	// Remove the service file
	servicePath := "/etc/systemd/system/citadel.service"
	if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove service file: %w", err)
	}
	fmt.Printf("✓ Removed %s\n", servicePath)

	// Reload systemd
	if err := runSvcCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}
	fmt.Println("✓ Reloaded systemd daemon")

	fmt.Println("\nCitadel system service has been uninstalled.")
	return nil
}

// macOS (launchd) implementation
func getSvcLaunchdPlistPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, "Library", "LaunchAgents", "ai.aceteam.citadel.plist")
}

func installDarwinSvc() error {
	// Get the path to the citadel binary
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	logDir := filepath.Join(homeDir, ".citadel-cli", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Create LaunchAgents directory if it doesn't exist
	launchAgentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return fmt.Errorf("failed to create LaunchAgents directory: %w", err)
	}

	// Create the plist file
	plistContent := fmt.Sprintf(launchdPlistTemplate,
		exePath,
		logDir,
		logDir,
		homeDir,
		homeDir,
	)

	plistPath := getSvcLaunchdPlistPath()
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}

	fmt.Printf("✓ Created launchd plist: %s\n", plistPath)

	// Load the service
	if err := runSvcCommand("launchctl", "load", plistPath); err != nil {
		return fmt.Errorf("failed to load service: %w", err)
	}
	fmt.Println("✓ Loaded citadel service")

	fmt.Println("\nCitadel is now running as a launchd service!")
	fmt.Println("\nUseful commands:")
	fmt.Println("  launchctl list ai.aceteam.citadel        - Check status")
	fmt.Println("  launchctl unload <plist>                 - Stop service")
	fmt.Println("  tail -f ~/.citadel-cli/logs/citadel.log  - View logs")

	return nil
}

func uninstallDarwinSvc() error {
	plistPath := getSvcLaunchdPlistPath()

	// Unload the service if loaded
	_ = runSvcCommand("launchctl", "unload", plistPath)
	fmt.Println("✓ Unloaded citadel service")

	// Remove the plist file
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist file: %w", err)
	}
	fmt.Printf("✓ Removed %s\n", plistPath)

	fmt.Println("\nCitadel launchd service has been uninstalled.")
	return nil
}

// Windows implementation
func installWindowsSvc() error {
	// Get the path to the citadel binary
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("failed to resolve executable path: %w", err)
	}

	// Check if running as admin
	if !isSvcWindowsAdmin() {
		return fmt.Errorf("installing a Windows service requires Administrator privileges.\nRight-click Command Prompt and select 'Run as administrator'")
	}

	// Create the service using sc.exe
	binPath := fmt.Sprintf("\"%s\" --daemon", exePath)

	// First try to delete existing service (ignore error)
	_ = runSvcCommand("sc", "delete", "citadel")

	// Create the service
	if err := runSvcCommand("sc", "create", "citadel",
		"binPath=", binPath,
		"DisplayName=", "Citadel - AceTeam Sovereign Compute Agent",
		"start=", "auto",
		"obj=", "LocalSystem",
	); err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}
	fmt.Println("✓ Created Windows service")

	// Set description
	_ = runSvcCommand("sc", "description", "citadel",
		"Citadel agent for the AceTeam Sovereign Compute Fabric. Connects this machine to your private AI infrastructure.")

	// Configure failure recovery (restart on failure)
	_ = runSvcCommand("sc", "failure", "citadel", "reset=", "86400", "actions=", "restart/60000/restart/60000/restart/60000")

	// Start the service
	if err := runSvcCommand("sc", "start", "citadel"); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}
	fmt.Println("✓ Started citadel service")

	fmt.Println("\nCitadel is now running as a Windows service!")
	fmt.Println("\nUseful commands:")
	fmt.Println("  sc query citadel     - Check status")
	fmt.Println("  sc stop citadel      - Stop service")
	fmt.Println("  sc start citadel     - Start service")
	fmt.Println("\nYou can also manage it from Services (services.msc)")

	return nil
}

func uninstallWindowsSvc() error {
	if !isSvcWindowsAdmin() {
		return fmt.Errorf("uninstalling a Windows service requires Administrator privileges.\nRight-click Command Prompt and select 'Run as administrator'")
	}

	// Stop the service
	_ = runSvcCommand("sc", "stop", "citadel")
	fmt.Println("✓ Stopped citadel service")

	// Delete the service
	if err := runSvcCommand("sc", "delete", "citadel"); err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}
	fmt.Println("✓ Deleted citadel service")

	fmt.Println("\nCitadel Windows service has been uninstalled.")
	return nil
}

func isSvcWindowsAdmin() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	// Try to open a file that requires admin rights
	_, err := os.Open("\\\\.\\PHYSICALDRIVE0")
	return err == nil
}

// Helper to run commands and show output
func runSvcCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Check if command exists
		if strings.Contains(err.Error(), "executable file not found") {
			return fmt.Errorf("command not found: %s", name)
		}
		return err
	}
	return nil
}
