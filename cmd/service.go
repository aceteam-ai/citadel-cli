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

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/services"
	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// forceRecreate controls whether to prompt user when containers exist
var forceRecreate bool

// composeEnv returns the process environment for a `docker compose` invocation
// in the cmd/ (CLI + TUI) tree, guaranteeing the citadel-owned host-port vars
// are present so compose templates that defer their host publish to the guarded
// ${CITADEL_*_HOST_PORT:?...} form (llamacpp/vllm/extraction/diffusers, #410)
// resolve.
//
// Before this helper, only the job-driven paths (internal/jobs) injected these
// vars; every cmd/ compose-up site (startService, ccStartService, `citadel run`)
// inherited a bare os.Environ() and so failed the :? guard on v2.57.0 for those
// four services. Every compose-up site in this tree MUST build its command
// environment from this helper. Mirrors internal/jobs.ServiceHandler.composeEnv.
func composeEnv() []string {
	return append(os.Environ(), svcports.HostPortEnv()...)
}

// composeCommand builds an *exec.Cmd for a `docker/podman compose` invocation
// against a citadel-owned compose file, guaranteeing the two invariants every
// cmd/ (CLI + TUI) compose call site needs:
//
//  1. The correct container runtime: it resolves catalog.SelectContainerRuntime()
//     (rootless podman preferred over docker, #348) and drives the invocation
//     through rt.Bin + rt.ComposeArgs(...), never a hardcoded "docker".
//  2. The citadel-owned host-port env (composeEnv -> services.HostPortEnv), so
//     compose templates that defer their host publish to the guarded
//     ${CITADEL_*_HOST_PORT:?...} form (llamacpp/vllm/extraction/diffusers, #410)
//     resolve instead of dying on the :? guard.
//
// Callers pass their compose args verbatim (e.g. "-f", path, "restart" or
// "-f", path, "-p", "citadel-"+name, "ps", "--format", "json") and invoke
// .Run()/.Output()/.CombinedOutput() on the result. Every cmd/ compose call site
// that interpolates a citadel compose file MUST route through this; a bare
// exec.Command("docker", "compose", ...) reintroduces the v2.57.0 regression
// where `citadel run --restart` and `citadel status` failed the :? guard (#426).
func composeCommand(args ...string) *exec.Cmd {
	rt := catalog.SelectContainerRuntime()
	cmd := exec.Command(rt.Bin, rt.ComposeArgs(args...)...)
	cmd.Env = composeEnv()
	return cmd
}

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
		filepath.Join(cacheBase, "sglang"),
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

	// Resolve the container runtime once (podman rootless preferred over docker;
	// see catalog.SelectContainerRuntime) and drive every sub-command below
	// through it, so module containers run under the hardened runtime when
	// available and fall back to docker otherwise (#348). Engine sub-commands
	// (inspect/rm) use rt.EngineBin (never the podman-compose wrapper); the
	// compose-up below uses rt.Bin + rt.ComposePrefix.
	rt := catalog.SelectContainerRuntime()
	fmt.Printf("   Container runtime: %s\n", rt.Label())

	// Check if container already exists and its state
	inspectCmd := exec.Command(rt.EngineBin, "inspect", "--format", "{{.State.Status}}", containerName)
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
			rmCmd := exec.Command(rt.EngineBin, "rm", "-f", containerName)
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
			rmCmd := exec.Command(rt.EngineBin, "rm", "-f", containerName)
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

	// Include the per-module least-privilege sandbox override when a sibling
	// <name>.sandbox.yml exists (written at install time for untrusted/Tier-2
	// modules). A no-op for every existing (non-sandboxed) service. The sibling is
	// derived from the ORIGINAL compose path, not actualComposePath -- on non-Linux
	// the latter may be a GPU-stripped temp file in a different directory.
	composeArgs := composeFileArgs(composeFilePath, actualComposePath)
	composeArgs = append(composeArgs, "up", "-d")
	args := rt.ComposeArgs(composeArgs...)
	composeCmd := exec.Command(rt.Bin, args...)
	// Supply the citadel-owned host ports so compose files that defer their host
	// publish to ${CITADEL_*_HOST_PORT:?...} (llamacpp/vllm/extraction/diffusers)
	// resolve. Without this, the :? guard added in #410 makes `docker compose up`
	// fail at this boot-path site (only the SERVICE_START job handler injected it
	// before). Mirrors internal/jobs.ServiceHandler.composeEnv (#426).
	composeCmd.Env = composeEnv()
	output, err = composeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s compose failed: %s", rt.Bin, string(output))
	}
	return nil
}

// composeFileArgs returns the ordered "-f <file>" arguments for a docker compose
// invocation: the base compose, followed by the per-module least-privilege
// sandbox override (<name>.sandbox.yml) when that sibling file exists. The
// sandbox sibling is resolved from origComposePath (the installed compose, whose
// directory holds the override), while the base file passed to docker is
// actualComposePath (which may be a platform-stripped temp copy). When no
// override exists, only the base file is returned -- additive and a no-op for
// every pre-sandbox service.
func composeFileArgs(origComposePath, actualComposePath string) []string {
	args := []string{"-f", actualComposePath}
	if override := sandboxOverridePathFor(origComposePath); override != "" {
		args = append(args, "-f", override)
	}
	return args
}

// sandboxOverridePathFor returns the path of the sandbox override that sits next
// to a service's compose file (<dir>/<name>.sandbox.yml), or "" if it does not
// exist. The compose file is "<name>.yml"; the override shares the same <name>.
// Delegates to catalog.ExistingSandboxOverride (the single source of truth for
// the override filename).
func sandboxOverridePathFor(composePath string) string {
	dir := filepath.Dir(composePath)
	base := filepath.Base(composePath)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	return catalog.ExistingSandboxOverride(dir, name)
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
	case "sglang":
		return "SGLang requires NVIDIA GPU (Linux only). Consider using Ollama on macOS."
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
