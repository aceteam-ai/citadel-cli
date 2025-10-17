// cmd/init.go
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/aceboss/citadel-cli/internal/ui"
	"github.com/aceboss/citadel-cli/services"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	authkey      string
	initService  string
	initNodeName string
	initTest     bool
)

type initTailscaleStatus struct {
	Self struct {
		Online bool `json:"Online"`
	} `json:"Self"`
}

const (
	netChoiceAuthkey  = "authkey"
	netChoiceBrowser  = "browser"
	netChoiceSkip     = "skip"
	netChoiceVerified = "verified"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Provisions a fresh server to become a Citadel Node",
	Long: `RUN WITH SUDO. This command installs all necessary dependencies, generates a
configuration based on your input, and brings the node online. It can be run
interactively or with flags for automation.`,
	Run: func(cmd *cobra.Command, args []string) {
		if !isRoot() {
			fmt.Fprintln(os.Stderr, "❌ Error: init command must be run with sudo.")
			os.Exit(1)
		}
		fmt.Println("✅ Running with root privileges.")

		// --- 1. Determine Configuration ---
		choice, key, err := getNetworkChoice()
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

		// --- Pre-flight check for running services ---
		if err := checkForRunningServices(selectedService); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Canceled: %v\n", err)
			os.Exit(1)
		}

		// --- 2. Generate Config Files ---
		originalUser := os.Getenv("SUDO_USER")
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

		// --- 3. Provision System ---
		provisionSteps := []struct {
			name     string
			checkCmd string
			run      func() error
		}{
			{"Package Lists", "", updateApt},
			{"Core Dependencies", "", installCoreDeps},
			{"Docker", "docker", installDocker},
			{"System User", "", setupUser},
			{"NVIDIA Container Toolkit", "nvidia-ctk", installNvidiaToolkit},
			{"Tailscale", "tailscale", installTailscale},
		}
		fmt.Println("--- 🚀 Starting Node Provisioning ---")
		for _, step := range provisionSteps {
			fmt.Printf("   - Processing step: %s\n", step.name)
			if step.checkCmd != "" && isCommandAvailable(step.checkCmd) {
				fmt.Printf("     ✅ Prerequisite '%s' is already installed. Skipping.\n", step.checkCmd)
			} else {
				if err := step.run(); err != nil {
					if step.name == "NVIDIA Container Toolkit" {
						fmt.Fprintf(os.Stderr, "     ⚠️  WARNING: Could not install %s. This is expected on non-GPU systems. Error: %v\n", step.name, err)
					} else {
						fmt.Fprintf(os.Stderr, "     ❌ FAILED: %s\n        Error: %v\n", step.name, err)
						os.Exit(1)
					}
				}
			}
		}
		fmt.Println("✅ System provisioning complete.")

		// --- 4. Final Handoff ---
		if choice == netChoiceSkip {
			fmt.Println("\n✅ Node is provisioned. Network connection was skipped.")
			fmt.Println("   To connect to the network later, run 'citadel login' or 'citadel up --authkey <key>'")
			return
		}

		if choice == netChoiceVerified {
			fmt.Println("\n✅ Node is provisioned. Since you are already logged in, services will start now.")
		} else {
			fmt.Println("\n✅ Node is provisioned. Now connecting to the network...")
		}

		executablePath, _ := os.Executable()
		var upArgs []string

		if choice == netChoiceBrowser {
			loginCmdStr := fmt.Sprintf("%s login --nexus %s", executablePath, nexusURL)
			loginCmd := exec.Command("sudo", "-u", originalUser, "sh", "-c", loginCmdStr)
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
			upCmd := exec.Command("sudo", "-u", originalUser, "sh", "-c", upCmdString)
			upCmd.Stdout = os.Stdout
			upCmd.Stderr = os.Stderr
			if err := upCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ 'citadel up' command failed during pre-test setup: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("\n--- 🔬 Running Power-On Self-Test (POST) for '%s' service ---\n", selectedService)
			testCommandString := fmt.Sprintf("cd %s && %s test --service %s", configDir, executablePath, selectedService)
			testCmd := exec.Command("sudo", "-u", originalUser, "sh", "-c", testCommandString)
			testCmd.Stdout = os.Stdout
			testCmd.Stderr = os.Stderr
			if err := testCmd.Run(); err != nil {
				os.Exit(1)
			}
		} else {
			fmt.Println("--- 🚀 Handing off to 'citadel up' to bring node online ---")
			upCommandString := fmt.Sprintf("cd %s && %s %s", configDir, executablePath, strings.Join(upArgs, " "))
			upCmd := exec.Command("sudo", "-u", originalUser, "sh", "-c", upCommandString)
			upCmd.Stdout = os.Stdout
			upCmd.Stderr = os.Stderr
			if err := upCmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "❌ 'citadel up' command failed: %v\n", err)
				os.Exit(1)
			}
		}
	},
}

func getNetworkChoice() (choice string, key string, err error) {
	if authkey != "" {
		fmt.Println("✅ Authkey provided via flag.")
		return netChoiceAuthkey, authkey, nil
	}

	fmt.Println("--- Checking network status...")
	statusCmd := exec.Command("tailscale", "status", "--json")
	output, _ := statusCmd.Output()

	var status initTailscaleStatus
	if json.Unmarshal(output, &status) == nil && status.Self.Online {
		fmt.Println("   - ✅ Already connected to the Nexus network.")
		return netChoiceVerified, "", nil
	}

	fmt.Println("   - ⚠️  You are not connected to the Nexus network.")
	// --- UPDATED CALL ---
	selection, err := ui.AskSelect(
		"How would you like to connect this node?",
		[]string{
			"Use a pre-generated authkey (Recommended for servers)",
			"Log in with a browser",
			"Skip network connection for now",
		},
	)
	if err != nil {
		return "", "", err
	}

	switch {
	case strings.Contains(selection, "authkey"):
		keyInput, err := ui.AskInput("Enter your Nexus authkey:", "nexus-auth-...", "")
		if err != nil {
			return "", "", err
		}
		return netChoiceAuthkey, strings.TrimSpace(keyInput), nil
	case strings.Contains(selection, "browser"):
		return netChoiceBrowser, "", nil
	default:
		return netChoiceSkip, "", nil
	}
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
	// --- UPDATED CALL ---
	selection, err := ui.AskSelect(
		"Which primary service should this node run?",
		[]string{
			"vllm (High-throughput OpenAI-compatible API)",
			"ollama (General purpose, easy to use)",
			"llamacpp (Versatile GGUF server)",
			"none (Connect to network only)",
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
	// --- UPDATED CALL ---
	return ui.AskInput("Enter a name for this node:", "e.g., gpu-node-1", defaultName)
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

// (The rest of the file is unchanged and omitted for brevity)
// ...
func createGlobalConfig(nodeConfigDir string) error {
	fmt.Println("--- Registering node configuration system-wide ---")
	globalConfigDir := "/etc/citadel"
	globalConfigFile := filepath.Join(globalConfigDir, "config.yaml")

	if err := os.MkdirAll(globalConfigDir, 0755); err != nil {
		return fmt.Errorf("failed to create global config directory %s: %w", globalConfigDir, err)
	}

	configContent := fmt.Sprintf("node_config_dir: %s\n", nodeConfigDir)

	if err := os.WriteFile(globalConfigFile, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("failed to write global config file %s: %w", globalConfigFile, err)
	}

	fmt.Printf("✅ Configuration registered at %s\n", globalConfigFile)
	return nil
}

func generateCitadelConfig(user, nodeName, serviceName string) (string, error) {
	fmt.Println("--- 📝 Generating configuration files ---")
	homeDir := "/home/" + user
	configDir := filepath.Join(homeDir, "citadel-node")
	servicesDir := filepath.Join(configDir, "services")

	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return "", err
	}

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

	manifestPath := filepath.Join(configDir, "citadel.yaml")
	if err := os.WriteFile(manifestPath, yamlData, 0644); err != nil {
		return "", err
	}

	userInfo, err := exec.Command("id", "-u", user).Output()
	if err != nil {
		return "", err
	}
	uid, _ := strconv.Atoi(strings.TrimSpace(string(userInfo)))

	groupInfo, err := exec.Command("id", "-g", user).Output()
	if err != nil {
		return "", err
	}
	gid, _ := strconv.Atoi(strings.TrimSpace(string(groupInfo)))

	err = filepath.Walk(configDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return syscall.Chown(path, uid, gid)
	})
	if err != nil {
		return "", fmt.Errorf("failed to chown config directory: %w", err)
	}

	fmt.Printf("✅ Configuration generated in %s\n", configDir)
	return configDir, nil
}

func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func isRoot() bool {
	return os.Geteuid() == 0
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("command '%s' failed: %s", cmd.String(), string(output))
	}
	return nil
}

func updateApt() error {
	fmt.Println("     - Running apt-get update...")
	return runCommand("apt-get", "update", "-qq")
}

func installCoreDeps() error {
	fmt.Println("     - Installing core dependencies (sudo, curl, gpg)...")
	return runCommand("apt-get", "install", "-y", "-qq", "sudo", "curl", "gpg", "ca-certificates")
}

func installDocker() error {
	fmt.Println("     - Running official Docker install script...")
	installScript := "curl -fsSL https://get.docker.com | sh"
	cmd := exec.Command("sh", "-c", installScript)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func setupUser() error {
	originalUser := os.Getenv("SUDO_USER")
	if originalUser == "" || originalUser == "root" {
		fmt.Println("     - No regular user detected ($SUDO_USER is empty or root). Skipping user setup.")
		return nil
	}

	checkCmd := exec.Command("id", "-u", originalUser)
	if err := checkCmd.Run(); err != nil {
		fmt.Printf("     - User '%s' not found. Creating user...\n", originalUser)
		if err := runCommand("useradd", "-m", "-s", "/bin/bash", "-G", "sudo", originalUser); err != nil {
			return fmt.Errorf("failed to create user %s: %w", originalUser, err)
		}
	} else {
		fmt.Printf("     - User '%s' already exists.\n", originalUser)
	}

	fmt.Printf("     - Ensuring user '%s' is in the 'docker' group...\n", originalUser)
	if err := runCommand("usermod", "-aG", "docker", originalUser); err != nil {
		return err
	}

	fmt.Printf("     - Granting passwordless sudo to user '%s'...\n", originalUser)
	sudoersFileContent := fmt.Sprintf("%s ALL=(ALL) NOPASSWD: ALL\n", originalUser)
	err := os.WriteFile(fmt.Sprintf("/etc/sudoers.d/99-citadel-%s", originalUser), []byte(sudoersFileContent), 0440)
	if err != nil {
		return fmt.Errorf("failed to configure passwordless sudo: %w", err)
	}

	return nil
}

func installNvidiaToolkit() error {
	fmt.Println("     - Running NVIDIA install script...")
	script := `curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg \
  && curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
    sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
    tee /etc/apt/sources.list.d/nvidia-container-toolkit.list > /dev/null \
  && apt-get update -qq \
  && apt-get install -y -qq nvidia-container-toolkit \
  && nvidia-ctk runtime configure --runtime=docker \
  && systemctl restart docker`
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func installTailscale() error {
	fmt.Println("     - Running Tailscale install script...")
	script := "curl -fsSL https://tailscale.com/install.sh | sh"
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
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
}
