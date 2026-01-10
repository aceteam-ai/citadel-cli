// cmd/init.go
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/aceboss/citadel-cli/internal/nexus"
	"github.com/aceboss/citadel-cli/internal/platform"
	"github.com/aceboss/citadel-cli/internal/ui"
	"github.com/aceboss/citadel-cli/services"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	authkey               string
	initService           string
	initNodeName          string
	initTest              bool
	initVerbose           bool
	userAddedToDockerGroup bool // Track if we added user to docker group in this run
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Provisions a fresh server to become a Citadel Node",
	Long: `RUN WITH SUDO. This command installs all necessary dependencies, generates a
configuration based on your input, and brings the node online. It can be run
interactively or with flags for automation.`,
	Example: `  # Interactive setup (recommended for first-time setup)
  sudo citadel init

  # Automated setup with specific service
  sudo citadel init --service vllm --node-name gpu-server-1

  # Setup with pre-generated authkey (for CI/CD)
  sudo citadel init --authkey <your-key> --service ollama

  # Setup with verbose output (for debugging)
  sudo citadel init --verbose

  # Setup without running tests
  sudo citadel init --test=false`,
	Run: func(cmd *cobra.Command, args []string) {
		if !isRoot() {
			fmt.Fprintln(os.Stderr, "❌ Error: init command must be run with sudo.")
			os.Exit(1)
		}
		fmt.Println("✅ Running with root privileges.")

		// Check if already connected to Tailscale and logout to start fresh
		if nexus.IsTailscaleConnected() {
			fmt.Println("--- 🔌 Existing Tailscale connection detected ---")
			if err := nexus.TailscaleLogout(); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Warning: %v\n", err)
				// Continue anyway - we'll try to connect with new credentials
			}
		}

		choice, key, err := nexus.GetNetworkChoice(authkey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
			os.Exit(1)
		}

		selectedService, err := getSelectedService()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
			os.Exit(1)
		}

		nodeName, err := getNodeName()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error getting node name: %v\n", err)
			os.Exit(1)
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
		configDir, err := generateCitadelConfig(originalUser, nodeName, selectedService)
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

		provisionSteps := []struct {
			name     string
			checkCmd string
			run      func() error
		}{
			{"Docker", "docker", installDocker},
			{"System User", "", setupUser},
			{"NVIDIA Container Toolkit", "nvidia-ctk", installNvidiaToolkit},
			{"Configure Docker for NVIDIA", "", configureNvidiaDocker},
			{"Tailscale", "tailscale", installTailscale},
		}

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

		// --- 3. Final Handoff ---
		// Only show Docker permissions warning if we actually added the user to the group
		if userAddedToDockerGroup && originalUser != "" && originalUser != "root" {
			fmt.Println("\n⚠️  IMPORTANT: For Docker permissions to apply, you must log out and log back in,")
			fmt.Printf("   or start a new login shell with: exec su -l %s\n", originalUser)
		}

		if choice == nexus.NetChoiceSkip {
			fmt.Println("\n✅ Node is provisioned. Network connection was skipped.")
			fmt.Println("   To connect to the network later, run 'citadel login' or 'citadel up --authkey <key>'")
			return
		}

		if choice == nexus.NetChoiceVerified {
			fmt.Println("\n✅ Node is provisioned. Since you are already logged in, services will start now.")
		} else {
			fmt.Println("\n✅ Node is provisioned. Now connecting to the network...")
		}

		executablePath, _ := os.Executable()
		var upArgs []string

		if choice == nexus.NetChoiceDevice {
			// Device authorization flow
			token, err := runDeviceAuthFlow(authServiceURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ %v\n", err)
				fmt.Fprintf(os.Stderr, "\nAlternative: Generate an authkey at %s/fabric\n", authServiceURL)
				fmt.Fprintln(os.Stderr, "Then run: citadel init --authkey <your-key>")
				os.Exit(1)
			}

			// Use the token as an authkey for the rest of the flow
			upArgs = []string{"up", "--authkey", token.Authkey}
		} else if choice == nexus.NetChoiceBrowser {
			loginCmdStr := fmt.Sprintf("%s login --nexus %s", executablePath, nexusURL)
			loginCmd := exec.Command("sudo", "-H", "-u", originalUser, "sh", "-c", loginCmdStr)
			loginCmd.Stdout = os.Stdout
			loginCmd.Stderr = os.Stderr
			loginCmd.Stdin = os.Stdin
			if err := loginCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ Login command failed. Please try again.\n")
				os.Exit(1)
			}
			upArgs = []string{"up"}
		} else {
			upArgs = []string{"up"}
			if key != "" {
				upArgs = append(upArgs, "--authkey", key)
			}
		}

		if initTest {
			fmt.Println("--- 🚀 Handing off to 'citadel up' to bring services online for testing ---")
			testUpArgs := append(upArgs, "--services-only")
			upCmdString := fmt.Sprintf("cd %s && %s %s", configDir, executablePath, strings.Join(testUpArgs, " "))
			upCmd := exec.Command("sudo", "-H", "-u", originalUser, "sh", "-c", upCmdString)
			upCmd.Stdout = os.Stdout
			upCmd.Stderr = os.Stderr
			if err := upCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ 'citadel up' command failed during pre-test setup: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("\n--- 🔬 Running Power-On Self-Test (POST) for '%s' service ---\n", selectedService)
			testCommandString := fmt.Sprintf("cd %s && %s test --service %s", configDir, executablePath, selectedService)
			testCmd := exec.Command("sudo", "-H", "-u", originalUser, "sh", "-c", testCommandString)
			testCmd.Stdout = os.Stdout
			testCmd.Stderr = os.Stderr
			if err := testCmd.Run(); err != nil {
				os.Exit(1)
			}
		} else {
			fmt.Println("--- 🚀 Handing off to 'citadel up' to bring node online ---")
			upCommandString := fmt.Sprintf("cd %s && %s %s", configDir, executablePath, strings.Join(upArgs, " "))
			upCmd := exec.Command("sudo", "-H", "-u", originalUser, "sh", "-c", upCommandString)
			upCmd.Stdout = os.Stdout
			upCmd.Stderr = os.Stderr
			if err := upCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ 'citadel up' command failed: %v\n", err)
				os.Exit(1)
			}
		}

		fmt.Println("\n--- ✅ Initialization Complete ---")
		fmt.Println("You can run 'citadel status' at any time to check the node's health.")
		fmt.Println("Running a final status check now...")

		statusCmdString := fmt.Sprintf("%s status", executablePath)
		statusCmd := exec.Command("sudo", "-H", "-u", originalUser, "sh", "-c", statusCmdString)
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
	defaultName, _ := os.Hostname()
	return ui.AskInput("Enter a name for this node:", "e.g., gpu-node-1", defaultName)
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

	if err := os.WriteFile(globalConfigFile, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("failed to write global config file %s: %w", globalConfigFile, err)
	}

	if initVerbose {
		fmt.Printf("✅ Configuration registered at %s\n", globalConfigFile)
	}
	return nil
}

func generateCitadelConfig(user, nodeName, serviceName string) (string, error) {
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
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return "", err
		}
	}

	manifest := CitadelManifest{
		Node: struct {
			Name string   `yaml:"name"`
			Tags []string `yaml:"tags"`
		}{
			Name: nodeName,
			Tags: []string{"gpu", "provisioned-by-citadel"},
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

	if err := os.WriteFile(manifestPath, yamlData, 0644); err != nil {
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

	// Grant passwordless sudo (Linux only - on macOS, this is handled differently)
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
			fmt.Println("     - Skipping NVIDIA Container Toolkit (not required on macOS).")
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
			fmt.Println("     - Skipping NVIDIA Docker configuration (not required on macOS).")
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

func installTailscale() error {
	if initVerbose {
		fmt.Println("     - Running Tailscale install script...")
	}

	script := "curl -fsSL https://tailscale.com/install.sh | sh"
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
}
