package platform

import (
	"fmt"
	"os/exec"
)

// DockerManager interface defines operations for Docker installation and management
type DockerManager interface {
	IsInstalled() bool
	Install() error
	Start() error
	EnsureUserInDockerGroup(username string) error
	ConfigureRuntime() error
}

// GetDockerManager returns the appropriate Docker manager for the current OS
func GetDockerManager() (DockerManager, error) {
	switch OS() {
	case "linux":
		return &LinuxDockerManager{}, nil
	case "darwin":
		return &DarwinDockerManager{}, nil
	default:
		return nil, fmt.Errorf("unsupported operating system: %s", OS())
	}
}

// LinuxDockerManager implements DockerManager for Linux systems
type LinuxDockerManager struct{}

func (l *LinuxDockerManager) IsInstalled() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

func (l *LinuxDockerManager) Install() error {
	if l.IsInstalled() {
		return nil // Already installed
	}

	fmt.Println("Installing Docker Engine...")
	// Use Docker's official installation script
	cmd := exec.Command("sh", "-c", "curl -fsSL https://get.docker.com | sh")
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install Docker: %w", err)
	}

	return nil
}

func (l *LinuxDockerManager) Start() error {
	// Start Docker using systemctl
	cmd := exec.Command("systemctl", "start", "docker")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start Docker: %w", err)
	}

	// Enable Docker to start on boot
	cmd = exec.Command("systemctl", "enable", "docker")
	return cmd.Run()
}

func (l *LinuxDockerManager) EnsureUserInDockerGroup(username string) error {
	userMgr, err := GetUserManager()
	if err != nil {
		return err
	}

	// Ensure docker group exists
	if err := userMgr.CreateGroup("docker", false); err != nil {
		return fmt.Errorf("failed to create docker group: %w", err)
	}

	// Add user to docker group if not already a member
	if !userMgr.IsUserInGroup(username, "docker") {
		if err := userMgr.AddUserToGroup(username, "docker"); err != nil {
			return fmt.Errorf("failed to add user to docker group: %w", err)
		}
	}

	return nil
}

func (l *LinuxDockerManager) ConfigureRuntime() error {
	// This is for NVIDIA runtime configuration on Linux
	// Check if nvidia-container-toolkit is needed
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		// No NVIDIA GPU, skip runtime configuration
		return nil
	}

	fmt.Println("Configuring NVIDIA Container Toolkit...")

	// Install nvidia-container-toolkit
	// This is distribution-specific, but we'll use the official method
	script := `
		distribution=$(. /etc/os-release;echo $ID$VERSION_ID)
		curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
		curl -s -L https://nvidia.github.io/libnvidia-container/$distribution/libnvidia-container.list | \
			sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
			tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
		apt-get update
		apt-get install -y nvidia-container-toolkit
		nvidia-ctk runtime configure --runtime=docker
		systemctl restart docker
	`

	cmd := exec.Command("sh", "-c", script)
	if err := cmd.Run(); err != nil {
		fmt.Println("Warning: Failed to configure NVIDIA runtime. This is OK if you don't have an NVIDIA GPU.")
		return nil // Don't fail on this
	}

	return nil
}

// DarwinDockerManager implements DockerManager for macOS Docker Desktop
type DarwinDockerManager struct{}

func (d *DarwinDockerManager) IsInstalled() bool {
	// Check if Docker Desktop is installed
	_, err := exec.LookPath("docker")
	return err == nil
}

func (d *DarwinDockerManager) Install() error {
	if d.IsInstalled() {
		return nil // Already installed
	}

	fmt.Println("Docker Desktop is not installed.")
	fmt.Println("Installing Docker Desktop via Homebrew...")

	// Ensure Homebrew is installed
	if err := EnsureHomebrew(); err != nil {
		return err
	}

	// Install Docker Desktop using Homebrew cask
	cmd := exec.Command("brew", "install", "--cask", "docker")
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install Docker Desktop: %w\n\nAlternatively, download Docker Desktop manually from: https://www.docker.com/products/docker-desktop", err)
	}

	fmt.Println("Docker Desktop installed successfully.")
	fmt.Println("Please start Docker Desktop from Applications and ensure it's running before continuing.")

	return nil
}

func (d *DarwinDockerManager) Start() error {
	// On macOS, Docker runs through Docker Desktop application
	// We can try to start it via open command
	cmd := exec.Command("open", "-a", "Docker")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start Docker Desktop: %w\nPlease start Docker Desktop manually from Applications", err)
	}

	fmt.Println("Docker Desktop is starting. Please wait for it to be fully running...")
	fmt.Println("You can check Docker's status by looking for the Docker icon in your menu bar.")

	return nil
}

func (d *DarwinDockerManager) EnsureUserInDockerGroup(username string) error {
	// Docker Desktop on macOS doesn't require group membership
	// Access is controlled by the Docker Desktop application itself
	return nil
}

func (d *DarwinDockerManager) ConfigureRuntime() error {
	// Docker Desktop on macOS handles GPU access differently
	// On Apple Silicon, it uses Metal framework
	// No additional configuration needed
	return nil
}
