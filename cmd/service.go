// cmd/service.go
// Shared service management helper functions
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/services"
)

// forceRecreate controls whether to prompt user when containers exist
var forceRecreate bool

// prepareCacheDirectories creates the cache directories for all services.
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

// startService starts a docker-based service using docker compose.
func startService(serviceName, composeFilePath string) error {
	if composeFilePath == "" {
		return fmt.Errorf("service %s has no compose_file defined", serviceName)
	}

	// Check for Mac-specific warnings
	if warning := macServiceWarning(serviceName); warning != "" {
		fmt.Printf("   ⚠️  %s\n", warning)
	}

	containerName := "citadel-" + serviceName

	// Check if container already exists and its state
	inspectCmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName)
	output, err := inspectCmd.Output()

	if err == nil {
		status := strings.TrimSpace(string(output))

		if status == "running" {
			// Container is already running - skip starting
			fmt.Printf("   ✅ Container %s is already running.\n", containerName)
			return nil
		}

		// Container exists but is stopped/exited/paused
		if forceRecreate {
			// Force mode: remove the old container and recreate
			fmt.Printf("   ♻️  Removing stale container %s...\n", containerName)
			rmCmd := exec.Command("docker", "rm", "-f", containerName)
			rmCmd.Run() // Ignore errors - container might already be gone
		} else {
			// Interactive mode: prompt user
			fmt.Printf("   ⚠️  Container %s exists but is %s.\n", containerName, status)
			fmt.Print("   Recreate container? (Y/n) ")
			var response string
			fmt.Scanln(&response)
			response = strings.TrimSpace(strings.ToLower(response))

			if response != "" && response != "y" && response != "yes" {
				return fmt.Errorf("container %s exists but is not running - user chose not to recreate", containerName)
			}

			// Remove the old container before recreating
			fmt.Printf("   ♻️  Removing stale container %s...\n", containerName)
			rmCmd := exec.Command("docker", "rm", "-f", containerName)
			rmCmd.Run() // Ignore errors
		}
	}

	// Start the service (container either doesn't exist or was just removed)
	actualComposePath := composeFilePath

	// On non-Linux platforms, strip GPU device reservations from compose file
	if !platform.IsLinux() {
		content, readErr := os.ReadFile(composeFilePath)
		if readErr == nil {
			filtered, filterErr := compose.StripGPUDevices(content)
			if filterErr == nil {
				// Write filtered content to a temp file (0600 for security)
				tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("citadel-compose-%s.yml", serviceName))
				if writeErr := os.WriteFile(tmpPath, filtered, 0600); writeErr == nil {
					actualComposePath = tmpPath
					defer os.Remove(tmpPath)
					fmt.Println("   ℹ️  Running in CPU-only mode (GPU acceleration unavailable on this platform)")
				}
			}
		}
	}

	composeCmd := exec.Command("docker", "compose", "-f", actualComposePath, "up", "-d")
	output, err = composeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose failed: %s", string(output))
	}
	return nil
}

// determineServiceType decides whether to use native or docker for a service.
func determineServiceType(service Service) services.ServiceType {
	// If explicitly set in manifest, use that
	if service.Type == "native" {
		return services.ServiceTypeNative
	}
	if service.Type == "docker" {
		return services.ServiceTypeDocker
	}

	// Auto-detect: prefer native if available
	if services.IsNativeAvailable(service.Name) {
		return services.ServiceTypeNative
	}

	// On macOS, strongly prefer native for Ollama (Docker has issues)
	if runtime.GOOS == "darwin" && service.Name == "ollama" {
		// Check if Ollama is installed via Homebrew or app
		if _, err := exec.LookPath("ollama"); err == nil {
			return services.ServiceTypeNative
		}
		// Suggest installing native Ollama
		fmt.Println("   ℹ️  Tip: Install native Ollama for best performance on macOS:")
		fmt.Println("      brew install ollama")
		fmt.Println("      or download from https://ollama.com/download")
	}

	// Fall back to docker
	return services.ServiceTypeDocker
}

// macServiceWarning returns a warning message for services that have known issues on macOS.
func macServiceWarning(serviceName string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}

	switch serviceName {
	case "lmstudio":
		return "LM Studio Docker image is Linux-only. Install the native macOS app from https://lmstudio.ai"
	case "vllm":
		return "vLLM requires NVIDIA GPU (Linux only). Consider using Ollama on macOS."
	case "llamacpp":
		return "llama.cpp Docker may have limited performance on macOS. Consider native installation."
	default:
		return ""
	}
}

// startNativeService starts a service using its native binary.
func startNativeService(serviceName, configDir string) error {
	// Check if already running
	if services.IsNativeServiceRunning(serviceName) {
		fmt.Printf("   ✅ Service %s is already running.\n", serviceName)
		return nil
	}

	// Get log directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not find home directory: %w", err)
	}
	logDir := filepath.Join(homeDir, "citadel-node", "logs")

	// Start the service
	process, err := services.StartNativeService(serviceName, logDir)
	if err != nil {
		return err
	}

	// Wait for service to be ready (max 30 seconds)
	fmt.Printf("   ⏳ Waiting for %s to be ready...\n", serviceName)
	if err := services.WaitForServiceReady(serviceName, 30*time.Second); err != nil {
		// Try to stop the process if it failed to become ready
		if process.Cmd.Process != nil {
			process.Cmd.Process.Kill()
		}
		return err
	}

	return nil
}
