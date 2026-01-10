package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
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
	case "windows":
		return &WindowsDockerManager{}, nil
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
	// Check if nvidia-container-toolkit is installed
	if _, err := exec.LookPath("nvidia-ctk"); err != nil {
		// NVIDIA Container Toolkit not installed, skip runtime configuration
		return nil
	}

	// Check if nvidia-smi exists (indicates NVIDIA GPU present)
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		// No NVIDIA GPU, skip runtime configuration
		return nil
	}

	// Configure the NVIDIA runtime for Docker
	script := `nvidia-ctk runtime configure --runtime=docker --set-as-default 2>/dev/null && systemctl restart docker 2>/dev/null`

	cmd := exec.Command("sh", "-c", script)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		// Don't fail if configuration doesn't work - might already be configured
		return nil
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

// WindowsDockerManager implements DockerManager for Windows systems with Docker Desktop
type WindowsDockerManager struct{}

func (w *WindowsDockerManager) IsInstalled() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

func (w *WindowsDockerManager) Install() error {
	if w.IsInstalled() {
		return nil // Already installed
	}

	// Check for WSL2 (required for Docker Desktop on Windows)
	if !w.hasWSL2() {
		fmt.Fprintln(os.Stderr, "⚠️  WSL2 is required for Docker Desktop on Windows.")
		fmt.Fprintln(os.Stderr, "Install WSL2 with: wsl --install")
		fmt.Fprintln(os.Stderr, "Then restart your computer and run citadel init again.")
		return fmt.Errorf("WSL2 not found - please install WSL2 first")
	}

	fmt.Println("Installing Docker Desktop for Windows...")
	pm, err := GetPackageManager()
	if err != nil {
		return err
	}

	if err := pm.Install("Docker.DockerDesktop"); err != nil {
		return fmt.Errorf("failed to install Docker Desktop: %w\n\nAlternatively, download Docker Desktop manually from: https://www.docker.com/products/docker-desktop", err)
	}

	fmt.Println("Docker Desktop installed successfully.")
	fmt.Println("Please start Docker Desktop and ensure it's running before continuing.")

	return nil
}

func (w *WindowsDockerManager) Start() error {
	// Check if Docker Desktop is already running
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err == nil {
		fmt.Println("Docker Desktop is already running.")
		return nil
	}

	// Start Docker Desktop
	dockerPath := `C:\Program Files\Docker\Docker\Docker Desktop.exe`
	if _, err := os.Stat(dockerPath); err != nil {
		return fmt.Errorf("Docker Desktop executable not found at %s\nPlease install Docker Desktop first", dockerPath)
	}

	fmt.Println("Starting Docker Desktop...")
	cmd = exec.Command(dockerPath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start Docker Desktop: %w\nPlease start Docker Desktop manually", err)
	}

	fmt.Println("Docker Desktop is starting. This may take a minute...")
	fmt.Println("Waiting for Docker to be ready...")

	// Wait for Docker to be ready (up to 60 seconds)
	for i := 0; i < 60; i++ {
		time.Sleep(1 * time.Second)
		cmd = exec.Command("docker", "info")
		if err := cmd.Run(); err == nil {
			fmt.Println("✅ Docker Desktop is ready!")
			return nil
		}
	}

	fmt.Println("⚠️  Docker Desktop is taking longer than expected to start.")
	fmt.Println("Please wait for Docker Desktop to finish starting, then continue.")
	return nil
}

func (w *WindowsDockerManager) EnsureUserInDockerGroup(username string) error {
	// Docker Desktop on Windows doesn't require group membership
	// Access is controlled by Docker Desktop via Windows ACLs
	return nil
}

func (w *WindowsDockerManager) ConfigureRuntime() error {
	// Docker Desktop on Windows handles runtime configuration automatically
	// For NVIDIA GPUs, Docker Desktop uses WSL2 with CUDA support
	// No manual configuration needed
	return nil
}

// hasWSL2 checks if WSL2 is installed on the Windows system
func (w *WindowsDockerManager) hasWSL2() bool {
	cmd := exec.Command("wsl", "--status")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	// Check if output contains "WSL 2" or "Default Version: 2"
	outputStr := string(output)
	return strings.Contains(outputStr, "WSL 2") || strings.Contains(outputStr, "Default Version: 2")
}

