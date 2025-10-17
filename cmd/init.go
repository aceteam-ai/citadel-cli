// cmd/init.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/AlecAivazis/survey/v2"
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

		// --- 4. Hand off to 'citadel up' ---
		fmt.Println("--- 🚀 Handing off to 'citadel up' to bring node online ---")
		executablePath, _ := os.Executable()
		upCommandString := fmt.Sprintf("cd %s && %s up --authkey %s", configDir, executablePath, authkey)
		upCmd := exec.Command("sudo", "-u", originalUser, "sh", "-c", upCommandString)
		upCmd.Stdout = os.Stdout
		upCmd.Stderr = os.Stderr
		if err := upCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ 'citadel up' command failed: %v\n", err)
			os.Exit(1)
		}

		// --- 5. Run Test if Requested ---
		if initTest {
			fmt.Println("\n--- 🔬 Handing off to 'citadel test' to verify node health ---")
			testCommandString := fmt.Sprintf("cd %s && %s test --service %s", configDir, executablePath, selectedService)
			testCmd := exec.Command("sudo", "-u", originalUser, "sh", "-c", testCommandString)
			testCmd.Stdout = os.Stdout
			testCmd.Stderr = os.Stderr
			if err := testCmd.Run(); err != nil {
				// The test command prints its own success/failure message, so we just exit.
				os.Exit(1)
			}
		}
	},
}

func getSelectedService() (string, error) {
	if initService != "" {
		// Validate the service provided by flag
		validServices := append(services.GetAvailableServices(), "none")
		for _, s := range validServices {
			if initService == s {
				fmt.Printf("✅ Using specified service: %s\n", initService)
				return initService, nil
			}
		}
		return "", fmt.Errorf("invalid service '%s' specified", initService)
	}

	// Interactive prompt
	prompt := &survey.Select{
		Message: "Which primary service should this node run?",
		Options: []string{
			"vllm (High-throughput OpenAI-compatible API)",
			"ollama (General purpose, easy to use)",
			"llamacpp (Versatile GGUF server)",
			"none (Connect to network only)",
		},
	}
	var selection string
	if err := survey.AskOne(prompt, &selection); err != nil {
		return "", err
	}
	// Extract the short name (e.g., "vllm" from "vllm (...)")
	return strings.Fields(selection)[0], nil
}

func getNodeName() (string, error) {
	if initNodeName != "" {
		fmt.Printf("✅ Using specified node name: %s\n", initNodeName)
		return initNodeName, nil
	}
	defaultName, err := os.Hostname()
	if err != nil {
		return "", err
	}
	prompt := &survey.Input{
		Message: "Enter a name for this node:",
		Default: defaultName,
	}
	var nodeName string
	if err := survey.AskOne(prompt, &nodeName); err != nil {
		return "", err
	}
	return nodeName, nil
}

func generateCitadelConfig(user, nodeName, serviceName string) (string, error) {
	fmt.Println("--- 📝 Generating configuration files ---")
	homeDir := "/home/" + user
	configDir := filepath.Join(homeDir, "citadel-node")
	servicesDir := filepath.Join(configDir, "services")

	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return "", err
	}

	// Generate service compose files
	for name, content := range services.ServiceMap {
		filePath := filepath.Join(servicesDir, name+".yml")
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return "", err
		}
	}

	// Generate citadel.yaml
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

	// IMPORTANT: Change ownership of the generated files to the original user
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

// Helper functions (isCommandAvailable, isRoot, runCommand, etc.) remain the same
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

	// 1. Ensure user exists
	checkCmd := exec.Command("id", "-u", originalUser)
	if err := checkCmd.Run(); err != nil {
		fmt.Printf("     - User '%s' not found. Creating user...\n", originalUser)
		if err := runCommand("useradd", "-m", "-s", "/bin/bash", "-G", "sudo", originalUser); err != nil {
			return fmt.Errorf("failed to create user %s: %w", originalUser, err)
		}
	} else {
		fmt.Printf("     - User '%s' already exists.\n", originalUser)
	}

	// 2. Ensure user is in the docker group
	fmt.Printf("     - Ensuring user '%s' is in the 'docker' group...\n", originalUser)
	if err := runCommand("usermod", "-aG", "docker", originalUser); err != nil {
		return err
	}

	// 3. Grant passwordless sudo to the user
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

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().StringVar(&authkey, "authkey", "", "The pre-authenticated key to join the network")
	initCmd.MarkFlagRequired("authkey")
	// New flags for automation
	initCmd.Flags().StringVar(&initService, "service", "", "Service to configure (vllm, ollama, llamacpp, none)")
	initCmd.Flags().StringVar(&initNodeName, "node-name", "", "Set the node name (defaults to hostname)")
	initCmd.Flags().BoolVar(&initTest, "test", false, "Run a diagnostic test after provisioning")
}
