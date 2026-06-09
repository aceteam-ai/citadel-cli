// cmd/app.go
// Modular app catalog commands for deploying developer tools on Citadel nodes.
package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/apps"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var appCmd = &cobra.Command{
	Use:     "app",
	Aliases: []string{"apps"},
	Short:   "Manage developer tool apps on this node",
	Long: `Install, manage, and remove developer tool applications on your Citadel node.

Apps run as Docker containers and are accessible via your browser. Data is
persisted in ~/.citadel/apps/<name>/data/ so it survives restarts.

Available apps: code-server, jupyter, filebrowser, ollama-webui`,
}

var appListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show available apps with install status",
	RunE:  runAppList,
}

var appInstallCmd = &cobra.Command{
	Use:   "install <name>",
	Short: "Deploy an app on this node",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppInstall,
}

var appStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show running apps and their ports",
	RunE:  runAppStatus,
}

var appStopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a running app",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppStop,
}

var appStartCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a previously stopped app",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppStart,
}

var appUninstallKeepData bool

var appUninstallCmd = &cobra.Command{
	Use:   "uninstall <name>",
	Short: "Remove an app, its container, and data",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppUninstall,
}

func init() {
	rootCmd.AddCommand(appCmd)
	appCmd.AddCommand(appListCmd)
	appCmd.AddCommand(appInstallCmd)
	appCmd.AddCommand(appStatusCmd)
	appCmd.AddCommand(appStopCmd)
	appCmd.AddCommand(appStartCmd)
	appCmd.AddCommand(appUninstallCmd)

	appUninstallCmd.Flags().BoolVar(&appUninstallKeepData, "keep-data", false,
		"Preserve the app's data directory instead of deleting it")
}

// runAppList shows the catalog with install status.
func runAppList(cmd *cobra.Command, args []string) error {
	state, err := apps.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load app state: %w", err)
	}

	ctx := context.Background()
	runner := apps.ExecRunner{}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	bold := color.New(color.Bold)
	bold.Fprintf(w, "NAME\tDESCRIPTION\tSTATUS\tPORT\n")

	for _, name := range apps.List() {
		manifest, _ := apps.Lookup(name)
		installed, isInstalled := state.Get(name)

		statusStr := color.New(color.FgWhite).Sprint("available")
		portStr := "-"

		if isInstalled {
			dockerStatus := apps.ContainerStatus(ctx, runner, name)
			switch dockerStatus {
			case "running":
				statusStr = color.New(color.FgGreen).Sprint("running")
			case "exited":
				statusStr = color.New(color.FgYellow).Sprint("stopped")
			default:
				statusStr = color.New(color.FgRed).Sprint(dockerStatus)
			}
			portStr = fmt.Sprintf("%d", installed.HostPort)
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, manifest.Description, statusStr, portStr)
	}

	return nil
}

// runAppInstall deploys an app.
func runAppInstall(cmd *cobra.Command, args []string) error {
	appName := args[0]

	manifest, ok := apps.Lookup(appName)
	if !ok {
		return fmt.Errorf("unknown app: %s\nRun 'citadel app list' to see available apps", appName)
	}

	ctx := context.Background()
	runner := apps.ExecRunner{}

	// Check Docker availability.
	if err := apps.DockerAvailable(ctx, runner); err != nil {
		return err
	}

	// Load state.
	state, err := apps.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load app state: %w", err)
	}

	// Check if already installed.
	if _, installed := state.Get(appName); installed {
		status := apps.ContainerStatus(ctx, runner, appName)
		if status == "running" {
			fmt.Printf("App %s is already installed and running.\n", appName)
			return nil
		}
		// Exists but not running — start it.
		fmt.Printf("App %s is installed but not running. Starting...\n", appName)
		if err := apps.Start(ctx, runner, appName); err != nil {
			return fmt.Errorf("failed to start app: %w", err)
		}
		fmt.Printf("Started %s.\n", appName)
		return nil
	}

	// Allocate a host port.
	hostPort, err := state.AllocatePort()
	if err != nil {
		return fmt.Errorf("failed to allocate port: %w", err)
	}

	// Create data directory.
	dataDir, err := apps.AppDataDir(appName)
	if err != nil {
		return fmt.Errorf("failed to determine data directory: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	// Create sub-directories for volumes.
	for _, vol := range manifest.Volumes {
		if vol.HostPath != "" {
			subDir := dataDir + "/" + vol.HostPath
			if err := os.MkdirAll(subDir, 0755); err != nil {
				return fmt.Errorf("failed to create volume directory %s: %w", subDir, err)
			}
		}
	}

	// Generate and inject auth password for apps that need it.
	generatedPassword := ""
	if apps.NeedsPassword(appName) {
		pw, err := apps.GeneratePassword(16)
		if err != nil {
			return fmt.Errorf("failed to generate password: %w", err)
		}
		generatedPassword = pw
		switch appName {
		case "code-server":
			manifest.Env["PASSWORD"] = pw
		case "filebrowser":
			manifest.Env["FB_PASSWORD"] = pw
			manifest.Env["FB_USERNAME"] = "admin"
		}
	}

	fmt.Printf("Installing %s...\n", appName)
	fmt.Printf("  Image: %s\n", manifest.Image)
	fmt.Printf("  Port:  localhost:%d\n", hostPort)
	fmt.Printf("  Data:  %s\n", dataDir)

	// Pull image with a timeout.
	fmt.Printf("  Pulling image...\n")
	pullCtx, pullCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer pullCancel()
	if _, err := runner.Run(pullCtx, "docker", "pull", manifest.Image); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", manifest.Image, err)
	}

	// Start the container.
	containerID, err := apps.Install(ctx, runner, manifest, hostPort, dataDir)
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Record in state. On failure, clean up the orphaned container.
	state.Set(apps.InstalledApp{
		Name:        appName,
		Image:       manifest.Image,
		ContainerID: containerID,
		HostPort:    hostPort,
	})
	if err := state.Save(); err != nil {
		_ = apps.Stop(ctx, runner, appName)
		_, _ = runner.Run(ctx, "docker", "rm", "-f", apps.ContainerName(appName))
		return fmt.Errorf("failed to save state (container cleaned up): %w", err)
	}

	// Wait for health check.
	fmt.Printf("  Waiting for app to be ready...")
	healthCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	if err := apps.WaitHealthy(healthCtx, manifest, hostPort); err != nil {
		fmt.Printf(" timeout\n")
		fmt.Printf("  Warning: %v\n", err)
		fmt.Printf("  The app may still be starting. Check with 'citadel app status'.\n")
	} else {
		fmt.Printf(" ready!\n")
	}

	fmt.Printf("\nApp %s is now available at http://localhost:%d\n", appName, hostPort)
	if generatedPassword != "" {
		fmt.Printf("  Password: %s\n", generatedPassword)
	}
	return nil
}

// runAppStatus shows running apps and their ports.
func runAppStatus(cmd *cobra.Command, args []string) error {
	state, err := apps.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load app state: %w", err)
	}

	if len(state.Apps) == 0 {
		fmt.Println("No apps installed. Run 'citadel app list' to see available apps.")
		return nil
	}

	ctx := context.Background()
	runner := apps.ExecRunner{}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	bold := color.New(color.Bold)
	bold.Fprintf(w, "NAME\tSTATUS\tPORT\tURL\n")

	for _, name := range apps.List() {
		installed, ok := state.Get(name)
		if !ok {
			continue
		}

		dockerStatus := apps.ContainerStatus(ctx, runner, name)
		var statusStr string
		switch dockerStatus {
		case "running":
			statusStr = color.New(color.FgGreen).Sprint("running")
		case "exited":
			statusStr = color.New(color.FgYellow).Sprint("stopped")
		default:
			statusStr = color.New(color.FgRed).Sprint(dockerStatus)
		}

		url := fmt.Sprintf("http://localhost:%d", installed.HostPort)
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", name, statusStr, installed.HostPort, url)
	}

	return nil
}

// runAppStop stops a running app.
func runAppStop(cmd *cobra.Command, args []string) error {
	appName := args[0]

	state, err := apps.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load app state: %w", err)
	}

	if _, ok := state.Get(appName); !ok {
		return fmt.Errorf("app %s is not installed", appName)
	}

	ctx := context.Background()
	runner := apps.ExecRunner{}

	fmt.Printf("Stopping %s...\n", appName)
	if err := apps.Stop(ctx, runner, appName); err != nil {
		return fmt.Errorf("failed to stop app: %w", err)
	}

	fmt.Printf("Stopped %s.\n", appName)
	return nil
}

// runAppStart starts a previously stopped app.
func runAppStart(cmd *cobra.Command, args []string) error {
	appName := args[0]

	state, err := apps.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load app state: %w", err)
	}

	if _, ok := state.Get(appName); !ok {
		return fmt.Errorf("app %s is not installed. Run 'citadel app install %s' first", appName, appName)
	}

	ctx := context.Background()
	runner := apps.ExecRunner{}

	fmt.Printf("Starting %s...\n", appName)
	if err := apps.Start(ctx, runner, appName); err != nil {
		return fmt.Errorf("failed to start app: %w", err)
	}

	fmt.Printf("Started %s.\n", appName)
	return nil
}

// runAppUninstall removes an app container, state, and data.
func runAppUninstall(cmd *cobra.Command, args []string) error {
	appName := args[0]

	state, err := apps.LoadState()
	if err != nil {
		return fmt.Errorf("failed to load app state: %w", err)
	}

	if _, ok := state.Get(appName); !ok {
		return fmt.Errorf("app %s is not installed", appName)
	}

	ctx := context.Background()
	runner := apps.ExecRunner{}

	fmt.Printf("Uninstalling %s...\n", appName)
	if err := apps.Uninstall(ctx, runner, appName); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	// Remove from state.
	state.Remove(appName)
	if err := state.Save(); err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}

	// Remove data directory unless --keep-data is set.
	appDir, _ := apps.AppDir(appName)
	if appUninstallKeepData {
		fmt.Printf("Uninstalled %s. Data preserved at %s.\n", appName, appDir)
	} else {
		if err := os.RemoveAll(appDir); err != nil {
			fmt.Printf("Warning: failed to remove data directory %s: %v\n", appDir, err)
		} else {
			fmt.Printf("Uninstalled %s and removed data at %s.\n", appName, appDir)
		}
	}
	return nil
}
