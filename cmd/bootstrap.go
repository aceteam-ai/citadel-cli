// cmd/bootstrap.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var authKey string

// bootstrapCmd represents the bootstrap command
var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Provisions a fresh Ubuntu server to become a Citadel Node",
	Long: `RUN WITH SUDO. This command installs all necessary dependencies (Docker,
NVIDIA drivers, Tailscale) and then brings the node online using the provided
authkey. It is idempotent and can be run multiple times.`,
	Run: func(cmd *cobra.Command, args []string) {
		if !isRoot() {
			fmt.Fprintln(os.Stderr, "❌ Error: bootstrap command must be run with sudo.")
			os.Exit(1)
		}
		fmt.Println("✅ Running with root privileges.")

		provisionSteps := []struct {
			name     string
			checkCmd string
			run      func() error
		}{
			{"Package Lists", "", updateApt}, // No check needed for apt update
			{"Core Dependencies", "", installCoreDeps},
			{"Docker", "docker", installDocker},
			{"Docker Service", "", startDockerDaemon},
			{"System User", "", setupUser},
			{"NVIDIA Container Toolkit", "nvidia-ctk", installNvidiaToolkit},
			{"Tailscale", "tailscale", installTailscale},
			{"Tailscale Service", "", startTailscaleDaemon},
			// The daemon-ready check is now handled by 'citadel up'
		}

		fmt.Println("--- 🚀 Starting Node Provisioning ---")
		for _, step := range provisionSteps {
			fmt.Printf("   - Processing step: %s\n", step.name)

			if step.checkCmd != "" && isCommandAvailable(step.checkCmd) {
				fmt.Printf("     ✅ Prerequisite '%s' is already installed. Skipping installation.\n", step.checkCmd)
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

		fmt.Println("--- 🚀 Handing off to 'citadel up' to bring node online ---")
		originalUser := os.Getenv("SUDO_USER")
		if originalUser == "" {
			fmt.Fprintln(os.Stderr, "❌ Could not determine the original user from $SUDO_USER.")
			os.Exit(1)
		}

		executablePath, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ Could not find path to citadel binary: %v\n", err)
			os.Exit(1)
		}

		// MODIFIED: Use `newgrp docker` to ensure the user's new group membership is active
		// for the 'citadel up' command, allowing it to access the docker socket.
		upCommandString := fmt.Sprintf("%s up --authkey %s", executablePath, authKey)
		upCmd := exec.Command("/usr/bin/sudo", "-u", originalUser, "newgrp", "docker", "-c", upCommandString)

		upCmd.Stdout = os.Stdout
		upCmd.Stderr = os.Stderr

		if err := upCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "❌ 'citadel up' command failed: %v\n", err)
			os.Exit(1)
		}
	},
}

// --- Helper Functions ---

func startDockerDaemon() error {
	fmt.Println("     - Starting Docker daemon (dockerd)...")
	// In a non-systemd environment (like a container), we need to start the daemon manually.
	cmd := exec.Command("sh", "-c", "dockerd > /dev/null 2>&1 &")
	return cmd.Run()
}

func startTailscaleDaemon() error {
	fmt.Println("     - Starting tailscaled service...")
	// The `&` sends the command to the background.
	cmd := exec.Command("sh", "-c", "tailscaled &")
	return cmd.Run()
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
	rootCmd.AddCommand(bootstrapCmd)
	bootstrapCmd.Flags().StringVar(&authKey, "authkey", "", "The pre-authenticated key to join the network")
	bootstrapCmd.MarkFlagRequired("authkey")
}
