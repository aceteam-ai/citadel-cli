// cmd/up.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aceboss/citadel-cli/internal/platform"
	"github.com/aceboss/citadel-cli/internal/terminal"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var servicesOnly bool

// upCmd represents the up command
var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Brings a Citadel Node online and starts its services from a manifest",
	Long: `Reads the citadel.yaml manifest, joins the network, and launches services.
In interactive mode, it checks for an existing login.
In automated mode (with --authkey), it joins the network non-interactively.`,
	Example: `  # Start services with existing network login
  citadel up

  # Start services with a new authkey (automated/CI)
  citadel up --authkey <your-key>`,

	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := waitForTailscaleDaemon(); err != nil {
			return err
		}
		fmt.Println("--- Verifying network status...")
		return checkTailscaleState()
	},

	Run: func(cmd *cobra.Command, args []string) {
		manifest, configDir, err := findAndReadManifest()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error loading configuration: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Manifest loaded for node: %s\n", manifest.Node.Name)

		if authkey != "" {
			fmt.Println("--- Establishing secure tunnel via authkey ---")
			err = joinNetwork(manifest.Node.Name, nexusURL, authkey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error joining network: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ Secure tunnel established.")
		} else {
			fmt.Println("✅ Network login verified.")
		}

		if err := prepareCacheDirectories(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error preparing cache: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("--- Launching services ---")
		for _, service := range manifest.Services {
			fullComposePath := filepath.Join(configDir, service.ComposeFile)
			fmt.Printf("🚀 Starting service: %s (%s)\n", service.Name, fullComposePath)
			err := startService(service.Name, fullComposePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to start service %s: %v\n", service.Name, err)
				os.Exit(1)
			}
			fmt.Printf("   ✅ Service %s is up.\n", service.Name)
		}

		fmt.Println("\n🎉 Citadel Node is online and services are running.")

		if servicesOnly {
			return // Exit before starting the agent
		}

		// Start terminal server if org-id is configured (non-Windows only)
		if manifest.Node.OrgID != "" && runtime.GOOS != "windows" {
			fmt.Println("--- Starting Terminal Server ---")
			termConfig := terminal.DefaultConfig()
			termConfig.OrgID = manifest.Node.OrgID
			termConfig.AuthServiceURL = authServiceURL

			termServer := terminal.NewServer(termConfig, terminal.NewHTTPTokenValidator(authServiceURL))
			if err := termServer.Start(); err != nil {
				fmt.Printf("   - ⚠️ Terminal server failed to start: %v\n", err)
			} else {
				fmt.Printf("   - ✅ Terminal server running on port %d\n", termConfig.Port)
			}
		}

		// Start the agent as the final step
		agentCmd.Run(cmd, args)
	},
}

// getTailscaleCLI returns the path to the tailscale CLI executable.
// On Windows, we need to use the full path because the PATH might not be updated
// in child processes (especially when launched via cmd /c from init).
func getTailscaleCLI() string {
	if platform.IsWindows() {
		// Standard installation path for Tailscale on Windows
		fullPath := `C:\Program Files\Tailscale\tailscale.exe`
		if _, err := os.Stat(fullPath); err == nil {
			return fullPath
		}
		// Fall back to PATH if the standard location doesn't exist
	}
	return "tailscale"
}

func waitForTailscaleDaemon() error {
	fmt.Println("--- Waiting for Network daemon to be ready...")

	// On macOS, we may need to start tailscaled manually
	if platform.IsDarwin() {
		if err := ensureTailscaledRunningMacOS(); err != nil {
			fmt.Printf("   - Warning: Could not start tailscaled: %v\n", err)
		}
	}

	// On Windows, we may need to start the Tailscale service
	if platform.IsWindows() {
		if err := ensureTailscaledRunningWindows(); err != nil {
			fmt.Printf("   - Warning: Could not start Tailscale service: %v\n", err)
		}
	}

	tailscaleCLI := getTailscaleCLI()
	maxAttempts := 10
	for i := 0; i < maxAttempts; i++ {
		cmd := exec.Command(tailscaleCLI, "status")
		output, err := cmd.CombinedOutput()
		outputStr := string(output)
		// If we get output (even "Tailscale is stopped" or "Logged out"), the daemon is responding
		// We only fail if the command itself errors without meaningful output
		if err == nil || strings.Contains(outputStr, "Logged out") || strings.Contains(outputStr, "stopped") {
			fmt.Println("✅ Daemon is ready.")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for tailscaled daemon to start")
}

// ensureTailscaledRunningMacOS attempts to start the tailscaled daemon on macOS
func ensureTailscaledRunningMacOS() error {
	// Check if tailscaled is already responding
	cmd := exec.Command("tailscale", "status")
	if err := cmd.Run(); err == nil {
		return nil // Already running
	}

	fmt.Println("   - Starting tailscaled on macOS...")

	// Try brew services first (if installed via Homebrew)
	brewCmd := exec.Command("brew", "services", "start", "tailscale")
	if err := brewCmd.Run(); err == nil {
		time.Sleep(1 * time.Second) // Give it a moment to start
		return nil
	}

	// Fall back to launchctl for standalone installation
	launchctlCmd := exec.Command("sudo", "launchctl", "load", "/Library/LaunchDaemons/com.tailscale.tailscaled.plist")
	if err := launchctlCmd.Run(); err == nil {
		time.Sleep(1 * time.Second)
		return nil
	}

	// Last resort: start tailscaled directly in the background
	// Note: This is less ideal as it won't persist across reboots
	tailscaledCmd := exec.Command("sudo", "tailscaled", "--state=/var/lib/tailscale/tailscaled.state", "--socket=/var/run/tailscale/tailscaled.sock")
	if err := tailscaledCmd.Start(); err != nil {
		return fmt.Errorf("could not start tailscaled: %w", err)
	}
	time.Sleep(1 * time.Second)
	return nil
}

// ensureTailscaledRunningWindows attempts to start the Tailscale service on Windows
func ensureTailscaledRunningWindows() error {
	// Check if Tailscale is already responding
	tailscaleCLI := getTailscaleCLI()
	cmd := exec.Command(tailscaleCLI, "status")
	if err := cmd.Run(); err == nil {
		return nil // Already running
	}

	fmt.Println("   - Starting Tailscale service on Windows...")

	// Try multiple approaches to start Tailscale

	// Approach 1: Try to start the Windows service
	// Note: This requires Administrator privileges
	startCmd := exec.Command("net", "start", "Tailscale")
	output, err := startCmd.CombinedOutput()
	outputStr := string(output)

	if err == nil {
		fmt.Println("     ✓ Tailscale service started successfully")
		time.Sleep(2 * time.Second)
		return nil
	}

	// Check if service is already running
	if strings.Contains(outputStr, "already") || strings.Contains(outputStr, "started") {
		fmt.Println("     ✓ Tailscale service is already running")
		time.Sleep(1 * time.Second)
		return nil
	}

	// If we get "Access is denied" or similar, we need elevation
	if strings.Contains(outputStr, "Access") || strings.Contains(outputStr, "denied") {
		fmt.Println("     ⚠️  Cannot start service without Administrator privileges")
		fmt.Println("     ℹ️  Attempting to launch Tailscale application...")

		// Approach 2: Try to launch the Tailscale GUI application
		// This can work without elevation and will start the service
		tailscaleExe := `C:\Program Files\Tailscale\tailscale-ipn.exe`
		if _, err := os.Stat(tailscaleExe); err == nil {
			appCmd := exec.Command(tailscaleExe)
			if err := appCmd.Start(); err == nil {
				fmt.Println("     ✓ Tailscale application launched")
				time.Sleep(3 * time.Second) // Give it more time to start the service
				return nil
			}
		}

		// Approach 3: Check if Tailscale was installed via winget to the user's local path
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData != "" {
			userTailscale := filepath.Join(localAppData, "Microsoft", "WinGet", "Packages", "Tailscale.Tailscale_Microsoft.Winget.Source_8wekyb3d8bbwe", "tailscale-ipn.exe")
			if _, err := os.Stat(userTailscale); err == nil {
				appCmd := exec.Command(userTailscale)
				if err := appCmd.Start(); err == nil {
					fmt.Println("     ✓ Tailscale application launched from user installation")
					time.Sleep(3 * time.Second)
					return nil
				}
			}
		}
	}

	// Log the actual error for debugging
	fmt.Printf("     ⚠️  Could not start Tailscale service: %s\n", outputStr)

	// Return the error but don't fail completely - the daemon check will retry
	return fmt.Errorf("could not start Tailscale: %w (output: %s)", err, outputStr)
}

func prepareCacheDirectories() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not find user home directory: %w", err)
	}

	cacheBase := filepath.Join(homeDir, "citadel-cache")
	// A list of all potential cache directories our services might use.
	dirsToCreate := []string{
		filepath.Join(cacheBase, "ollama"),
		filepath.Join(cacheBase, "vllm"),
		filepath.Join(cacheBase, "llamacpp"),
		filepath.Join(cacheBase, "lmstudio"),
		filepath.Join(cacheBase, "huggingface"),
	}

	fmt.Println("--- Preparing cache directories ---")
	// First, create the base directory
	if err := os.MkdirAll(cacheBase, 0755); err != nil {
		return fmt.Errorf("failed to create base cache directory %s: %w", cacheBase, err)
	}

	// Then create all the subdirectories
	for _, dir := range dirsToCreate {
		// 0655 permissions are rwx for user, group, and others.
		// This solves the Docker volume permission issue for the container user.
		if err := os.MkdirAll(dir, 0655); err != nil {
			return fmt.Errorf("failed to create cache directory %s: %w", dir, err)
		}
	}

	fmt.Println("✅ Cache directories are ready.")
	return nil
}

func joinNetwork(hostname, serverURL, key string) error {
	fmt.Printf("   - Bringing network up...\n")
	tailscaleCLI := getTailscaleCLI()

	// Logout first (ignore errors)
	if platform.IsWindows() {
		exec.Command(tailscaleCLI, "logout").Run()
	} else {
		exec.Command("sudo", tailscaleCLI, "logout").Run()
	}

	// Build the tailscale up command
	var tsCmd *exec.Cmd
	if platform.IsWindows() {
		// On Windows, we're already running as Administrator
		tsCmd = exec.Command(tailscaleCLI, "up",
			"--login-server="+serverURL,
			"--authkey="+key,
			"--hostname="+hostname,
			"--accept-routes",
			"--accept-dns",
		)
	} else {
		// On Linux/macOS, use sudo
		tsCmd = exec.Command("sudo", tailscaleCLI, "up",
			"--login-server="+serverURL,
			"--authkey="+key,
			"--hostname="+hostname,
			"--accept-routes",
			"--accept-dns",
		)
	}
	output, err := tsCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Network up failed: %s", string(output))
	}
	if !strings.Contains(string(output), "Success") {
		fmt.Printf("   - Network output: %s\n", string(output))
	}
	return nil
}

func checkTailscaleState() error {
	tailscaleCLI := getTailscaleCLI()
	cmd := exec.Command(tailscaleCLI, "status")
	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	// "Tailscale is stopped" or "Logged out" means daemon is responding but not connected
	// This is fine if we have an authkey - we'll connect with it
	isStopped := strings.Contains(outputStr, "stopped") || strings.Contains(outputStr, "Stopped")
	isLoggedOut := strings.Contains(outputStr, "Logged out")

	if err != nil && !isLoggedOut && !isStopped {
		return fmt.Errorf("tailscale daemon is not responding: %s", outputStr)
	}

	// If no authkey provided, we need to already be connected
	if authkey == "" {
		if isLoggedOut || isStopped {
			return fmt.Errorf("you are not logged into Network. Please run 'citadel login' or use an --authkey")
		}
	}
	return nil
}

func readManifest(filePath string) (*CitadelManifest, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("could not read file %s: %w", filePath, err)
	}
	var manifest CitadelManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("could not parse YAML in %s: %w", filePath, err)
	}
	return &manifest, nil
}

func startService(serviceName, composeFilePath string) error {
	if composeFilePath == "" {
		return fmt.Errorf("service %s has no compose_file defined", serviceName)
	}
	composeCmd := exec.Command("docker", "compose", "-f", composeFilePath, "up", "-d")
	output, err := composeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose failed: %s", string(output))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(upCmd)
	upCmd.Flags().StringVar(&authkey, "authkey", "", "The pre-authenticated key to join the network (for automation)")
	upCmd.Flags().BoolVar(&servicesOnly, "services-only", false, "Only start services and exit (internal use for init)")
	upCmd.Flags().MarkHidden("services-only")
}
