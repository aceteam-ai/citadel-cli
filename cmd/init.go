// cmd/init.go
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/ui"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	authkey                string
	initService            string
	initNodeName           string
	initTest               bool
	initVerbose            bool
	initNetworkOnly        bool // Deprecated: kept for backwards compatibility
	initProvision          bool
	initRelogin            bool
	initNewDevice          bool // Force fresh registration, ignoring existing machine mapping
	userAddedToDockerGroup bool // Track if we added user to docker group in this run
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Provisions a fresh server to become a Citadel Node",
	Long: `Provisions a fresh server to become a Citadel Node. This command connects
the node to the AceTeam Network.

By default, citadel init only joins the network (no sudo required). Services
can be configured via the AceTeam web management page.

Use --provision for full provisioning including Docker installation, NVIDIA toolkit,
and system user configuration (requires sudo).`,
	Example: `  # Default: join network only (no sudo required)
  citadel init

  # Full provisioning with Docker and NVIDIA toolkit (requires sudo)
  sudo citadel init --provision

  # Full provisioning with specific service
  sudo citadel init --provision --service vllm

  # Setup with pre-generated authkey (for CI/CD)
  citadel init --authkey <your-key>

  # Full provisioning with verbose output (for debugging)
  sudo citadel init --provision --verbose`,
	Run: func(cmd *cobra.Command, args []string) {
		// Root is only required for full provisioning (--provision flag)
		// Default mode only joins the network using embedded tsnet (no root required)
		if initProvision && !isRoot() {
			if platform.IsWindows() {
				fmt.Fprintln(os.Stderr, "❌ Error: init --provision must be run as Administrator.")
				fmt.Fprintln(os.Stderr, "   Right-click Command Prompt or PowerShell and select 'Run as administrator'")
				fmt.Fprintln(os.Stderr, "   (Remove --provision to just join the network without elevation)")
			} else {
				fmt.Fprintln(os.Stderr, "❌ Error: init --provision must be run with sudo.")
				fmt.Fprintln(os.Stderr, "   (Remove --provision to just join the network without sudo)")
			}
			os.Exit(1)
		}
		if initProvision {
			fmt.Println("✅ Running with root privileges.")
		}

		// Wire up debug logging for nexus package
		nexus.DebugFunc = Debug

		Debug("auth-service: %s", authServiceURL)
		Debug("nexus: %s", nexusURL)
		Debug("config dir: %s", platform.ConfigDir())

		// Handle --relogin: force fresh authentication while preserving network identity
		// We disconnect but keep tsnet state (machine key) to preserve IP address
		if initRelogin {
			Debug("--relogin flag set, forcing fresh authentication")
			if network.IsGlobalConnected() {
				fmt.Print("Disconnecting... ")
				// Use Disconnect() instead of Logout() to preserve tsnet state (machine key)
				// This allows Headscale to recognize the same node and preserve the IP
				if err := network.Disconnect(); err != nil {
					fmt.Printf("warning: %v\n", err)
				} else {
					fmt.Println("done")
				}
			}
			// Clear saved config to force fresh auth (but NOT network state)
			clearSavedConfig()
			Debug("cleared saved config (network state preserved for IP retention)")
		}

		Debug("checking network status...")
		Debug("IsGlobalConnected: %v", network.IsGlobalConnected())
		Debug("HasState: %v", network.HasState())

		choice, key, err := nexus.GetNetworkChoice(authkey)
		Debug("network choice: %s", choice)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
			os.Exit(1)
		}

		// If device authorization was selected, run the flow immediately
		// This shows the authorization box first, before other setup prompts
		var deviceAuthResult *DeviceAuthResult
		var earlyNetworkConnected bool
		var nodeName string // Declare early for reuse throughout init

		if choice == nexus.NetChoiceDevice {
			Debug("starting device authorization flow...")
			deviceAuthResult, err = runDeviceAuthFlow(authServiceURL, initNewDevice)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ %v\n", err)
				fmt.Fprintf(os.Stderr, "\nAlternative: Generate an authkey at %s/fabric\n", authServiceURL)
				fmt.Fprintln(os.Stderr, "Then run: citadel init --authkey <your-key>")
				os.Exit(1)
			}

			Debug("device auth successful")
			Debug("token response - authkey: %s...", deviceAuthResult.Token.Authkey[:min(20, len(deviceAuthResult.Token.Authkey))])
			Debug("token response - redis_url: %s", deviceAuthResult.Token.RedisURL)
			Debug("token response - device_api_token: %s", maskToken(deviceAuthResult.Token.DeviceAPIToken))
			Debug("token response - api_base_url: %s", deviceAuthResult.Token.APIBaseURL)
			Debug("token response - nexus_url: %s", deviceAuthResult.Token.NexusURL)
			Debug("token response - org_id: %s", deviceAuthResult.Token.OrgID)

			// Save device API token if provided (preferred secure mode)
			if deviceAuthResult.Token.DeviceAPIToken != "" {
				Debug("saving device API token to config...")
				if err := saveDeviceConfigToFile(deviceAuthResult.Token); err != nil {
					fmt.Fprintf(os.Stderr, "⚠️  Warning: Could not save device config: %v\n", err)
				} else {
					Debug("device API token saved successfully")
				}
			} else if deviceAuthResult.Token.RedisURL != "" {
				// Fallback: Save Redis URL to config if device_api_token not provided
				Debug("saving redis URL to config (legacy mode)...")
				if err := saveRedisURLToConfig(deviceAuthResult.Token.RedisURL); err != nil {
					fmt.Fprintf(os.Stderr, "⚠️  Warning: Could not save Redis URL to config: %v\n", err)
				} else {
					Debug("redis URL saved successfully")
				}
			} else {
				Debug("no device config in token response")
			}

			// Immediately connect to network after device auth succeeds
			fmt.Println("\n--- 🌐 Connecting to AceTeam Network ---")

			nodeName, err = getNodeName()
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error getting node name: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Connecting as '%s'...\n", nodeName)
			if err := connectToNetwork(nodeName, deviceAuthResult.Token.Authkey); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Failed to connect to network: %v\n", err)
				os.Exit(1)
			}
			ip, _ := network.GetGlobalIPv4()
			printNetworkSuccessInfo(nodeName, ip)
			earlyNetworkConnected = true

			fmt.Println("\n--- Continuing with node setup ---")
		}

		// Also connect early when authkey is provided via flag
		if choice == nexus.NetChoiceAuthkey && key != "" && !earlyNetworkConnected {
			fmt.Println("\n--- 🌐 Connecting to AceTeam Network ---")

			nodeName, err = getNodeName()
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error getting node name: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Connecting as '%s'...\n", nodeName)
			if err := connectToNetwork(nodeName, key); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Failed to connect to network: %v\n", err)
				os.Exit(1)
			}
			ip, _ := network.GetGlobalIPv4()
			printNetworkSuccessInfo(nodeName, ip)
			earlyNetworkConnected = true

			fmt.Println("\n--- Continuing with node setup ---")
		}

		// Default mode: join network and exit (use --provision for full provisioning)
		if !initProvision {
			// If we already connected early, just exit
			if earlyNetworkConnected {
				fmt.Printf("Node name: %s\n", nodeName)
				return
			}

			// If already connected via existing credentials, check if fully configured
			if choice == nexus.NetChoiceVerified {
				// Check if device config is present (API token or Redis URL)
				if !hasDeviceConfigured() {
					fmt.Println("⚠️  Device config not found. Authenticating...")
					deviceAuthResult, err = runDeviceAuthFlow(authServiceURL, initNewDevice)
					if err != nil {
						fmt.Fprintf(os.Stderr, "❌ %v\n", err)
						os.Exit(1)
					}

					// Save device config from device auth response
					if deviceAuthResult.Token.DeviceAPIToken != "" {
						if err := saveDeviceConfigToFile(deviceAuthResult.Token); err != nil {
							fmt.Fprintf(os.Stderr, "⚠️  Could not save config: %v\n", err)
						}
					} else if deviceAuthResult.Token.RedisURL != "" {
						if err := saveRedisURLToConfig(deviceAuthResult.Token.RedisURL); err != nil {
							fmt.Fprintf(os.Stderr, "⚠️  Could not save config: %v\n", err)
						}
					}
				}

				// Get node name from network status
				if nodeName == "" {
					nodeName, _ = getNodeName()
				}

				// Get IP address
				ip, _ := network.GetGlobalIPv4()

				// Already connected - show brief info
				fmt.Printf("✅ Connected as %s\n", nodeName)
				if ip != "" {
					fmt.Printf("   IP: %s\n", ip)
				}
				fmt.Println("\n   Run 'citadel status' to see network details and peers.")
				fmt.Println("   To switch accounts: citadel logout && citadel init")
				return
			}

			// Get node name if not already set
			if nodeName == "" {
				nodeName, err = getNodeName()
				if err != nil {
					fmt.Fprintf(os.Stderr, "❌ Error getting node name: %v\n", err)
					os.Exit(1)
				}
			}

			// Determine authkey to use
			var authkeyToUse string
			if choice == nexus.NetChoiceDevice && deviceAuthResult != nil {
				authkeyToUse = deviceAuthResult.Token.Authkey
			} else if key != "" {
				authkeyToUse = key
			}

			if authkeyToUse != "" {
				fmt.Printf("Connecting to AceTeam Network as '%s'...\n", nodeName)
				if err := connectToNetwork(nodeName, authkeyToUse); err != nil {
					fmt.Fprintf(os.Stderr, "❌ Failed to connect to network: %v\n", err)
					os.Exit(1)
				}
				ip, _ := network.GetGlobalIPv4()
				printNetworkSuccessInfo(nodeName, ip)
			} else if choice != nexus.NetChoiceSkip {
				fmt.Println("⚠️  No authkey available. Run 'citadel login' to complete network setup.")
			}

			fmt.Printf("Node name: %s\n", nodeName)
			return
		}

		selectedService, err := getSelectedService()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
			os.Exit(1)
		}

		// Get node name if not already set during early network connection
		if nodeName == "" {
			nodeName, err = getNodeName()
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ Error getting node name: %v\n", err)
				os.Exit(1)
			}
		}

		if !initVerbose {
			if err := checkForRunningServicesQuiet(selectedService); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
				os.Exit(1)
			}
		} else {
			if err := checkForRunningServices(selectedService); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
				os.Exit(1)
			}
		}

		originalUser := platform.GetSudoUser()
		if originalUser == "" {
			fmt.Fprintln(os.Stderr, "❌ Could not determine the original user from $SUDO_USER.")
			os.Exit(1)
		}
		// Extract org-id from device auth token if available
		orgID := ""
		if deviceAuthResult != nil && deviceAuthResult.Token != nil {
			orgID = deviceAuthResult.Token.OrgID
		}
		configDir, err := generateCitadelConfig(originalUser, nodeName, selectedService, orgID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to generate configuration files: %v\n", err)
			os.Exit(1)
		}
		if err := createGlobalConfig(configDir); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to create global system configuration: %v\n", err)
			os.Exit(1)
		}

		// --- 2. Provision System ---
		if !initVerbose {
			fmt.Println("--- 🚀 Provisioning node...")
		} else {
			fmt.Println("--- 🚀 Starting Node Provisioning (verbose mode) ---")
		}

		// First ensure core dependencies are installed
		if err := ensureCoreDependencies(); err != nil {
			fmt.Fprintf(os.Stderr, "     ❌ FAILED: Core Dependencies\n        Error: %v\n", err)
			os.Exit(1)
		}

		// Build provision steps based on selected service
		// If service is "none", skip Docker-related steps
		provisionSteps := []struct {
			name     string
			checkCmd string
			run      func() error
		}{}

		// Docker and related steps are only needed for containerized services
		if selectedService != "none" {
			provisionSteps = append(provisionSteps,
				struct {
					name     string
					checkCmd string
					run      func() error
				}{"Docker", "docker", installDocker},
			)
			provisionSteps = append(provisionSteps,
				struct {
					name     string
					checkCmd string
					run      func() error
				}{"System User", "", setupUser},
			)
			provisionSteps = append(provisionSteps,
				struct {
					name     string
					checkCmd string
					run      func() error
				}{"NVIDIA Container Toolkit", "nvidia-ctk", installNvidiaToolkit},
			)
			provisionSteps = append(provisionSteps,
				struct {
					name     string
					checkCmd string
					run      func() error
				}{"Configure Docker for NVIDIA", "", configureNvidiaDocker},
			)
		}

		// Note: Network connectivity is now handled via embedded tsnet library
		// No external Tailscale installation is needed

		for _, step := range provisionSteps {
			if initVerbose {
				fmt.Printf("   - Processing step: %s\n", step.name)
			}

			if step.checkCmd != "" && isCommandAvailable(step.checkCmd) {
				if initVerbose {
					fmt.Printf("     ✅ '%s' is already installed. Skipping.\n", step.checkCmd)
				}
			} else {
				if err := step.run(); err != nil {
					if step.name == "NVIDIA Container Toolkit" {
						if initVerbose {
							fmt.Fprintf(os.Stderr, "     ⚠️  WARNING: Could not install %s. This is expected on non-GPU systems. Error: %v\n", step.name, err)
						}
					} else {
						fmt.Fprintf(os.Stderr, "     ❌ FAILED: %s\n        Error: %v\n", step.name, err)
						os.Exit(1)
					}
				}
			}
		}
		fmt.Println("✅ System provisioning complete.")

		// Clarify what was skipped
		if selectedService == "none" {
			fmt.Println("   ℹ️  Docker installation was skipped (service=none).")
		}

		// --- 3. Final Handoff ---
		// Only show Docker permissions warning if we actually added the user to the group
		if userAddedToDockerGroup && originalUser != "" && originalUser != "root" {
			fmt.Println("\n⚠️  IMPORTANT: For Docker permissions to apply, you must log out and log back in,")
			fmt.Printf("   or start a new login shell with: exec su -l %s\n", originalUser)
		}

		if choice == nexus.NetChoiceSkip {
			fmt.Println("\n✅ Node is provisioned. Network connection was skipped.")
			fmt.Println("   To connect to the network later, run 'citadel login' or 'citadel login --authkey <key>'")
			return
		}

		if choice == nexus.NetChoiceVerified || earlyNetworkConnected {
			fmt.Println("\n✅ Node is provisioned. Network is connected, services will start now.")
		} else {
			fmt.Println("\n✅ Node is provisioned. Now connecting to the network...")
		}

		executablePath, _ := os.Executable()

		// Join the network if we have an authkey and haven't connected early
		if !earlyNetworkConnected {
			var authKey string
			if choice == nexus.NetChoiceDevice {
				authKey = deviceAuthResult.Token.Authkey
			} else if key != "" {
				authKey = key
			}

			if authKey != "" {
				fmt.Println("--- 🌐 Joining network ---")
				loginArgs := []string{"login", "--authkey", authKey, "--node-name", nodeName}
				loginCmdString := fmt.Sprintf("%s %s", executablePath, strings.Join(loginArgs, " "))
				loginCmd := exec.Command("sudo", "sh", "-c", loginCmdString)
				loginCmd.Stdout = os.Stdout
				loginCmd.Stderr = os.Stderr
				if err := loginCmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr, "❌ 'citadel login' command failed: %v\n", err)
					os.Exit(1)
				}
			}
		}

		// Start services with --force to avoid interactive prompts
		runArgs := []string{"run", "--force"}

		if initTest {
			fmt.Println("--- 🚀 Starting services for testing ---")
			runCmdString := fmt.Sprintf("cd %s && %s %s", configDir, executablePath, strings.Join(runArgs, " "))
			runCmd := runAsUser(originalUser, runCmdString)
			runCmd.Stdout = os.Stdout
			runCmd.Stderr = os.Stderr
			if err := runCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ 'citadel run' command failed during pre-test setup: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("\n--- 🔬 Running Power-On Self-Test (POST) for '%s' service ---\n", selectedService)
			testCommandString := fmt.Sprintf("cd %s && %s test --service %s", configDir, executablePath, selectedService)
			testCmd := runAsUser(originalUser, testCommandString)
			testCmd.Stdout = os.Stdout
			testCmd.Stderr = os.Stderr
			if err := testCmd.Run(); err != nil {
				os.Exit(1)
			}
		} else {
			fmt.Println("--- 🚀 Starting services ---")
			runCommandString := fmt.Sprintf("cd %s && %s %s", configDir, executablePath, strings.Join(runArgs, " "))
			runCmd := runAsUser(originalUser, runCommandString)
			runCmd.Stdout = os.Stdout
			runCmd.Stderr = os.Stderr
			if err := runCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ 'citadel run' command failed: %v\n", err)
				os.Exit(1)
			}
		}

		fmt.Println("\n--- ✅ Initialization Complete ---")
		fmt.Println("You can run 'citadel status' at any time to check the node's health.")
		fmt.Println("Running a final status check now...")

		statusCmdString := fmt.Sprintf("%s status", executablePath)
		statusCmd := runAsUser(originalUser, statusCmdString)
		statusCmd.Stdout = os.Stdout
		statusCmd.Stderr = os.Stderr
		if err := statusCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "\n⚠️  Could not run final status check: %v\n", err)
			// Do not exit, as the main init process was successful.
		}
	},
}

func getSelectedService() (string, error) {
	if initService != "" {
		validServices := append(services.GetAvailableServices(), "none")
		for _, s := range validServices {
			if initService == s {
				return initService, nil
			}
		}
		return "", fmt.Errorf("invalid service '%s' specified", initService)
	}
	selection, err := ui.AskSelect(
		"Which primary service should this node run?",
		[]string{
			"none (Connect to network only)",
			"vllm (High-throughput OpenAI-compatible API)",
			"ollama (General purpose, easy to use)",
			"llamacpp (Versatile GGUF server)",
		},
	)
	if err != nil {
		return "", err
	}
	return strings.Fields(selection)[0], nil
}

func getNodeName() (string, error) {
	if initNodeName != "" {
		return initNodeName, nil
	}
	// Use hostname by default without prompting
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("could not determine hostname: %w", err)
	}
	return hostname, nil
}

func checkForRunningServicesQuiet(serviceToStart string) error {
	cmd := exec.Command("docker", "ps", "--filter", "name=citadel-", "--format", "json")
	output, err := cmd.Output()
	if err != nil {
		// Could not query Docker, skip check silently
		return nil
	}

	var runningServices []DockerPsResult
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var res DockerPsResult
		if err := decoder.Decode(&res); err == nil {
			if !strings.Contains(res.Name, serviceToStart) {
				runningServices = append(runningServices, res)
			}
		}
	}

	// No conflicting services - continue silently
	if len(runningServices) == 0 {
		return nil
	}

	// Found conflicting services - show warning
	fmt.Println("⚠️  Found other Citadel services running. It's recommended to run only one AI service at a time.")
	fmt.Print("Stop these services before proceeding? (y/N) ")
	var response string
	fmt.Scanln(&response)
	if strings.ToLower(strings.TrimSpace(response)) != "y" {
		return nil
	}

	// Stop services quietly
	for _, s := range runningServices {
		stopCmd := exec.Command("docker", "stop", s.Name)
		if err := stopCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not stop %s: %v\n", s.Name, err)
		}
	}
	return nil
}

func checkForRunningServices(serviceToStart string) error {
	fmt.Println("--- 🔍 Checking for existing services ---")
	cmd := exec.Command("docker", "ps", "--filter", "name=citadel-", "--format", "json")
	output, err := cmd.Output()
	if err != nil {
		fmt.Println("     - Could not query Docker, skipping check.")
		return nil
	}

	var runningServices []DockerPsResult
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var res DockerPsResult
		if err := decoder.Decode(&res); err == nil {
			if !strings.Contains(res.Name, serviceToStart) {
				runningServices = append(runningServices, res)
			}
		}
	}

	if len(runningServices) == 0 {
		fmt.Println("     ✅ No other conflicting Citadel services are running.")
		return nil
	}

	fmt.Println("     ⚠️  Found other resource-intensive Citadel services running:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "     \tCONTAINER NAME\tIMAGE")
	fmt.Fprintln(w, "     \t--------------\t-----")
	for _, s := range runningServices {
		fmt.Fprintf(w, "     \t%s\t%s\n", s.Name, s.Image)
	}
	w.Flush()
	fmt.Println("     It is highly recommended to run only one AI service at a time to conserve GPU memory.")

	confirm := false
	fmt.Print("     Do you want to stop these services before proceeding? (y/N) ")
	var response string
	fmt.Scanln(&response)
	if strings.ToLower(strings.TrimSpace(response)) == "y" {
		confirm = true
	}

	if !confirm {
		fmt.Println("     - Skipping service shutdown. Proceeding with initialization.")
		return nil
	}

	fmt.Println("     - Stopping services...")
	for _, s := range runningServices {
		fmt.Printf("       - Stopping %s...\n", s.Name)
		stopCmd := exec.Command("docker", "stop", s.Name)
		if err := stopCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "       - ⚠️  Could not stop %s: %v\n", s.Name, err)
		}
	}
	fmt.Println("     ✅ Services stopped.")
	return nil
}

func createGlobalConfig(nodeConfigDir string) error {
	if initVerbose {
		fmt.Println("--- Registering node configuration system-wide ---")
	}

	globalConfigDir := platform.ConfigDir()
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")

	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create global config directory %s: %w", globalConfigDir, err)
	}

	configContent := fmt.Sprintf("node_config_dir: %s\n", nodeConfigDir)

	if err := os.WriteFile(globalConfigFile, []byte(configContent), 0600); err != nil {
		return fmt.Errorf("failed to write global config file %s: %w", globalConfigFile, err)
	}

	if initVerbose {
		fmt.Printf("✅ Configuration registered at %s\n", globalConfigFile)
	}
	return nil
}

// hasRedisURLConfigured checks if a Redis URL is stored in the global config.
// Deprecated: use hasDeviceConfigured instead.
func hasRedisURLConfigured() bool {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return false
	}

	var config struct {
		RedisURL string `yaml:"redis_url"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return false
	}
	return config.RedisURL != ""
}

// hasDeviceConfigured checks if device authentication config is present.
// Returns true if either device_api_token or redis_url is configured.
func hasDeviceConfigured() bool {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		return false
	}

	var config struct {
		DeviceAPIToken string `yaml:"device_api_token"`
		RedisURL       string `yaml:"redis_url"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return false
	}
	return config.DeviceAPIToken != "" || config.RedisURL != ""
}

// clearSavedConfig removes the saved config file to force fresh authentication.
func clearSavedConfig() {
	globalConfigFile := filepath.Join(platform.ConfigDir(), "config.yaml")

	// Read existing config to preserve node_config_dir
	data, err := os.ReadFile(globalConfigFile)
	if err != nil {
		// File doesn't exist, nothing to clear
		return
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		// Can't parse, remove it entirely
		_ = os.Remove(globalConfigFile)
		return
	}

	// Preserve node_config_dir, clear device-specific fields
	nodeConfigDir, hasNodeConfigDir := config["node_config_dir"]

	// Clear device auth fields
	delete(config, "device_api_token")
	delete(config, "api_base_url")
	delete(config, "org_id")
	delete(config, "redis_url")
	delete(config, "user_email")
	delete(config, "user_name")

	// If only node_config_dir remains (or nothing), write it back
	if hasNodeConfigDir {
		config["node_config_dir"] = nodeConfigDir
	}

	if len(config) == 0 {
		_ = os.Remove(globalConfigFile)
		return
	}

	// Write back preserved config
	newData, err := yaml.Marshal(config)
	if err != nil {
		_ = os.Remove(globalConfigFile)
		return
	}
	_ = os.WriteFile(globalConfigFile, newData, 0600)
}

// maskToken masks a token for safe logging, showing only first/last few chars.
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}

// saveDeviceConfigToFile saves device authentication config to the global config file.
// This is called after device auth to store the device_api_token and api_base_url.
func saveDeviceConfigToFile(token *nexus.TokenResponse) error {
	globalConfigDir := platform.ConfigDir()
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")

	// Ensure config directory exists
	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create global config directory: %w", err)
	}

	// Read existing config if it exists
	var config map[string]interface{}
	data, err := os.ReadFile(globalConfigFile)
	if err == nil {
		if unmarshalErr := yaml.Unmarshal(data, &config); unmarshalErr != nil {
			config = nil
		}
	}
	if config == nil {
		config = make(map[string]interface{})
	}

	// Add device API token, base URL, and org ID (secure mode)
	config["device_api_token"] = token.DeviceAPIToken
	if token.APIBaseURL != "" {
		config["api_base_url"] = token.APIBaseURL
	}
	if token.OrgID != "" {
		config["org_id"] = token.OrgID
	}
	if token.UserEmail != "" {
		config["user_email"] = token.UserEmail
	}
	if token.UserName != "" {
		config["user_name"] = token.UserName
	}
	// Remove redis_url - not needed when using API mode
	delete(config, "redis_url")

	// Write back
	newData, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(globalConfigFile, newData, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}

// saveRedisURLToConfig saves the Redis URL to the global config file.
// Deprecated: use saveDeviceConfigToFile instead.
// This is called after device auth to store the org-specific Redis endpoint.
func saveRedisURLToConfig(redisURL string) error {
	globalConfigDir := platform.ConfigDir()
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")

	// Ensure config directory exists
	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create global config directory: %w", err)
	}

	// Read existing config if it exists
	var config map[string]interface{}
	data, err := os.ReadFile(globalConfigFile)
	if err == nil {
		if unmarshalErr := yaml.Unmarshal(data, &config); unmarshalErr != nil {
			// If existing config is malformed, start fresh
			config = nil
		}
	}
	if config == nil {
		config = make(map[string]interface{})
	}

	// Add Redis URL
	config["redis_url"] = redisURL

	// Write back
	newData, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(globalConfigFile, newData, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}

func generateCitadelConfig(user, nodeName, serviceName, orgID string) (string, error) {
	if initVerbose {
		fmt.Println("--- 📝 Generating configuration files ---")
	}

	homeDir, err := platform.HomeDir(user)
	if err != nil {
		return "", fmt.Errorf("failed to get home directory for user %s: %w", user, err)
	}
	configDir := filepath.Join(homeDir, "citadel-node")
	cacheDir := filepath.Join(homeDir, "citadel-cache")
	servicesDir := filepath.Join(configDir, "services")
	manifestPath := filepath.Join(configDir, "citadel.yaml")

	// Clean up previous service configurations to ensure a fresh start.
	os.RemoveAll(servicesDir)

	// Create the main config directory and the services subdirectory.
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create services directory: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Always write all available service definitions to allow for easy switching later.
	for name, content := range services.ServiceMap {
		filePath := filepath.Join(servicesDir, name+".yml")
		if err := os.WriteFile(filePath, []byte(content), 0600); err != nil {
			return "", err
		}
	}

	manifest := CitadelManifest{
		Node: struct {
			Name  string   `yaml:"name"`
			Tags  []string `yaml:"tags"`
			OrgID string   `yaml:"org_id,omitempty"`
		}{
			Name:  nodeName,
			Tags:  []string{"gpu", "provisioned-by-citadel"},
			OrgID: orgID,
		},
	}

	// Only add the selected service to the manifest if one was chosen.
	if serviceName != "none" {
		manifest.Services = []Service{
			{
				Name:        serviceName,
				ComposeFile: filepath.Join("./services", serviceName+".yml"),
			},
		}
	}

	yamlData, err := yaml.Marshal(&manifest)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(manifestPath, yamlData, 0600); err != nil {
		return "", err
	}

	// Set correct ownership of config and cache directories
	for _, dir := range []string{configDir, cacheDir} {
		if err := platform.ChownR(dir, user); err != nil {
			return "", fmt.Errorf("failed to change ownership of directory %s: %w", dir, err)
		}
	}

	if initVerbose {
		fmt.Printf("✅ Configuration generated in %s\n", configDir)
	} else {
		fmt.Printf("✅ Configuration generated at %s\n", configDir)
	}
	return configDir, nil
}

func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func isRoot() bool {
	return platform.IsRoot()
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("command '%s' failed: %s", cmd.String(), string(output))
	}
	return nil
}

// runAsUser executes a command as a specific user in a cross-platform way
// On Linux/macOS: uses sudo -H -u <user> sh -c <command>
// On Windows: runs directly (already running as Administrator)
func runAsUser(user string, cmdString string) *exec.Cmd {
	if platform.IsWindows() {
		// On Windows, run directly with cmd.exe
		// The user is already running as Administrator
		// Use cmd /c to execute the command string (supports && syntax)
		return exec.Command("cmd", "/c", cmdString)
	}
	// On Linux/macOS, use sudo to run as the original user
	return exec.Command("sudo", "-H", "-u", user, "sh", "-c", cmdString)
}

func ensureCoreDependencies() error {
	pm, err := platform.GetPackageManager()
	if err != nil {
		return err
	}

	// On macOS, ensure Homebrew is installed first
	if platform.IsDarwin() {
		if err := platform.EnsureHomebrew(); err != nil {
			return fmt.Errorf("failed to ensure Homebrew is installed: %w", err)
		}
	}

	// On Windows, check for winget availability
	if platform.IsWindows() {
		if _, err := exec.LookPath("winget"); err != nil {
			return fmt.Errorf("winget not found - please upgrade to Windows 10 1809+ or Windows 11")
		}
		// curl is built-in on Windows 10 1803+, no additional packages needed
		if initVerbose {
			fmt.Println("     - Core dependencies already available on Windows.")
		}
		return nil
	}

	// Check which packages are needed
	var packages []string
	var missingPackages []string

	if platform.IsLinux() {
		packages = []string{"sudo", "curl", "gpg", "ca-certificates"}
	} else if platform.IsDarwin() {
		packages = []string{"curl", "gpg"} // sudo is built-in on macOS
	}

	for _, pkg := range packages {
		if !pm.IsInstalled(pkg) {
			missingPackages = append(missingPackages, pkg)
		}
	}

	// Only run apt update and install if there are missing packages
	if len(missingPackages) > 0 {
		if initVerbose {
			fmt.Printf("     - Updating package manager (%s)...\n", platform.OS())
		}
		if err := pm.Update(); err != nil {
			return fmt.Errorf("failed to update package manager: %w", err)
		}

		if initVerbose {
			fmt.Printf("     - Installing missing dependencies: %s\n", strings.Join(missingPackages, ", "))
		}
		return pm.Install(missingPackages...)
	}

	if initVerbose {
		fmt.Println("     - Core dependencies already installed.")
	}
	return nil
}

func installDocker() error {
	if initVerbose {
		fmt.Println("     - Installing Docker...")
	}

	dockerMgr, err := platform.GetDockerManager()
	if err != nil {
		return err
	}

	// Install Docker
	if err := dockerMgr.Install(); err != nil {
		return fmt.Errorf("failed to install Docker: %w", err)
	}

	// Start Docker (on Linux, this starts the daemon; on macOS, this opens Docker Desktop)
	if err := dockerMgr.Start(); err != nil {
		return fmt.Errorf("failed to start Docker: %w", err)
	}

	// Ensure user has Docker access
	originalUser := platform.GetSudoUser()
	if originalUser != "" && originalUser != "root" {
		// Check if user is already in docker group before adding
		if platform.IsLinux() {
			userMgr, err := platform.GetUserManager()
			if err == nil && !userMgr.IsUserInGroup(originalUser, "docker") {
				userAddedToDockerGroup = true
			}
		}

		if err := dockerMgr.EnsureUserInDockerGroup(originalUser); err != nil {
			return fmt.Errorf("failed to configure Docker access for user: %w", err)
		}
	}

	if initVerbose {
		if platform.IsLinux() {
			fmt.Println("\n✅ Docker installed and user configured successfully!")
			fmt.Println("--------------------------------------------------------------------------")
			fmt.Println("IMPORTANT: To apply the new group membership, you must do one of the following:")
			fmt.Println("\n  Option 1 (Recommended): Log out of your system and log back in.")
			fmt.Println("     OR")
			fmt.Println("  Option 2 (Immediate, for this terminal only): Run the following command:")
			fmt.Printf("     newgrp docker\n")
			fmt.Println("\nAfter applying the changes, you can verify that Docker runs without sudo:")
			fmt.Println("     docker run hello-world")
			fmt.Println("--------------------------------------------------------------------------")
		} else if platform.IsDarwin() {
			fmt.Println("\n✅ Docker Desktop installed successfully!")
			fmt.Println("--------------------------------------------------------------------------")
			fmt.Println("IMPORTANT: Please ensure Docker Desktop is running before continuing.")
			fmt.Println("You can verify Docker is running with:")
			fmt.Println("     docker ps")
			fmt.Println("--------------------------------------------------------------------------")
		}
	}

	return nil
}

func setupUser() error {
	originalUser := platform.GetSudoUser()
	if originalUser == "" || originalUser == "root" {
		if initVerbose {
			fmt.Println("     - No regular user detected ($SUDO_USER is empty or root). Skipping user setup.")
		}
		return nil
	}

	userMgr, err := platform.GetUserManager()
	if err != nil {
		return err
	}

	// Check if user exists, create if not
	if !userMgr.UserExists(originalUser) {
		if initVerbose {
			fmt.Printf("     - User '%s' not found. Creating user...\n", originalUser)
		}
		if err := userMgr.CreateUser(originalUser, false); err != nil {
			return fmt.Errorf("failed to create user %s: %w", originalUser, err)
		}

		// On Linux, add to sudo group
		if platform.IsLinux() {
			if err := userMgr.AddUserToGroup(originalUser, "sudo"); err != nil {
				if initVerbose {
					fmt.Printf("     - Warning: Could not add user to sudo group: %v\n", err)
				}
			}
		}
	} else if initVerbose {
		fmt.Printf("     - User '%s' already exists.\n", originalUser)
	}

	// Ensure user is in docker group (Linux only - Docker Desktop on macOS doesn't use a docker group)
	if platform.IsLinux() {
		if initVerbose {
			fmt.Printf("     - Ensuring user '%s' is in the 'docker' group...\n", originalUser)
		}
		if !userMgr.IsUserInGroup(originalUser, "docker") {
			if err := userMgr.AddUserToGroup(originalUser, "docker"); err != nil {
				return fmt.Errorf("failed to add user to docker group: %w", err)
			}
			// Track that we added the user to docker group in this run
			userAddedToDockerGroup = true
		}
	}

	// Grant passwordless sudo (Linux only - on macOS/Windows, this is handled differently)
	if platform.IsLinux() {
		if initVerbose {
			fmt.Printf("     - Granting passwordless sudo to user '%s'...\n", originalUser)
		}
		sudoersFileContent := fmt.Sprintf("%s ALL=(ALL) NOPASSWD: ALL\n", originalUser)
		err := os.WriteFile(fmt.Sprintf("/etc/sudoers.d/99-citadel-%s", originalUser), []byte(sudoersFileContent), 0440)
		if err != nil {
			return fmt.Errorf("failed to configure passwordless sudo: %w", err)
		}
	}

	return nil
}

func installNvidiaToolkit() error {
	// NVIDIA Container Toolkit is Linux-only
	if !platform.IsLinux() {
		if initVerbose {
			fmt.Println("     - Skipping NVIDIA Container Toolkit (not required on macOS/Windows).")
		}
		return nil
	}

	if initVerbose {
		fmt.Println("     - Installing NVIDIA Container Toolkit...")
	}

	// Remove old keyring if it exists to avoid prompts, then install toolkit
	script := `rm -f /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg \
  && curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg \
  && curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
    sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
    tee /etc/apt/sources.list.d/nvidia-container-toolkit.list > /dev/null \
  && apt-get update -qq \
  && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nvidia-container-toolkit`
	cmd := exec.Command("sh", "-c", script)

	if initVerbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = nil
		cmd.Stderr = nil
	}

	return cmd.Run()
}

// configureNvidiaDocker ensures the Docker daemon is correctly configured to use the NVIDIA runtime.
func configureNvidiaDocker() error {
	// NVIDIA runtime configuration is Linux-only
	if !platform.IsLinux() {
		if initVerbose {
			fmt.Println("     - Skipping NVIDIA Docker configuration (not required on macOS/Windows).")
		}
		return nil
	}

	if initVerbose {
		fmt.Println("     - Configuring Docker for NVIDIA runtime...")
	}

	dockerMgr, err := platform.GetDockerManager()
	if err != nil {
		return err
	}

	return dockerMgr.ConfigureRuntime()
}

// connectToNetwork connects to the AceTeam Network using the embedded tsnet library.
// No external Tailscale installation is required.
func connectToNetwork(nodeName, authKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	config := network.ServerConfig{
		Hostname:   nodeName,
		ControlURL: nexusURL, // From root.go
		AuthKey:    authKey,
	}

	srv, err := network.Connect(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	ip, _ := srv.GetIPv4()
	if ip != "" {
		fmt.Printf("   IP: %s\n", ip)
	}

	return nil
}

type DockerPsResult struct {
	Name  string `json:"Names"`
	Image string `json:"Image"`
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().StringVar(&authkey, "authkey", "", "The pre-authenticated key to join the network")
	initCmd.Flags().StringVar(&initService, "service", "", "Service to configure (vllm, ollama, llamacpp, none)")
	initCmd.Flags().StringVar(&initNodeName, "node-name", "", "Set the node name (defaults to hostname)")
	initCmd.Flags().BoolVar(&initTest, "test", true, "Run a diagnostic test after provisioning")
	initCmd.Flags().BoolVar(&initVerbose, "verbose", false, "Show detailed output during provisioning")
	initCmd.Flags().BoolVar(&initProvision, "provision", false, "Full provisioning with Docker, NVIDIA toolkit, and services (requires sudo)")
	initCmd.Flags().BoolVar(&initRelogin, "relogin", false, "Force re-authentication while preserving IP address")
	initCmd.Flags().BoolVar(&initNewDevice, "new-device", false, "Force fresh registration, ignoring existing machine mapping")
	// Deprecated: --network-only is now the default behavior
	initCmd.Flags().BoolVar(&initNetworkOnly, "network-only", false, "Deprecated: network-only is now the default")
	initCmd.Flags().MarkHidden("network-only")
}
