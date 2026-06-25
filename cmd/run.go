// cmd/run.go
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/services"

	"github.com/spf13/cobra"
)

var detachRun bool
var restartServices bool

// runCmd represents the run command
var runCmd = &cobra.Command{
	Use:   "run [service]",
	Short: "Start services (all if no service specified, or a specific one)",
	Long: fmt.Sprintf(`Starts services and adds them to the manifest for tracking.

When a service name is provided, it is added to the manifest (if not already present)
and started. When no service is specified, all services in the manifest are started.

Use --restart to restart running services instead of starting fresh.

Note: For production use, consider 'citadel work' which starts services AND runs
the job worker in a single command.

Available services: %s`, strings.Join(services.GetAvailableServices(), ", ")),
	Example: `  # Start a specific service (adds to manifest)
  citadel run vllm

  # Start all services in the manifest
  citadel run

  # Restart all services
  citadel run --restart

  # Start in foreground mode
  citadel run ollama --detach=false

  # Recommended: Start services + worker together
  citadel work --mode=nexus`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if restartServices {
			// Restart mode
			restartAllServices()
			return
		}

		if len(args) == 0 {
			// No service specified - start all manifest services
			runAllServices()
			return
		}

		// Start specific service
		serviceName := args[0]
		runSingleService(serviceName)
	},
}

// runAllServices starts all services defined in the manifest.
func runAllServices() {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		fmt.Fprintln(os.Stderr, "   Hint: Run 'citadel run <service>' to create a configuration.")
		os.Exit(1)
	}

	if len(manifest.Services) == 0 {
		fmt.Println("No services configured in manifest.")
		fmt.Println("   Hint: Run 'citadel run <service>' to add and start a service.")
		return
	}

	fmt.Printf("--- 🚀 Starting %d service(s) ---\n", len(manifest.Services))

	for _, service := range manifest.Services {
		serviceType := determineServiceType(service)

		if serviceType == internalServices.ServiceTypeNative {
			fmt.Printf("🚀 Starting service: %s (native)\n", service.Name)
			if err := startNativeService(service.Name, configDir); err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to start service %s: %v\n", service.Name, err)
				fmt.Fprintf(os.Stderr, "   Hint: Run 'citadel logs %s' to see detailed output.\n", service.Name)
				os.Exit(1)
			}
		} else {
			// Validate that compose file path stays within config directory (prevent path traversal)
			fullComposePath, err := platform.ValidatePathWithinDir(configDir, service.ComposeFile)
			if err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Invalid compose file path for %s: %v\n", service.Name, err)
				os.Exit(1)
			}
			fmt.Printf("🚀 Starting service: %s\n", service.Name)
			if err := startService(service.Name, fullComposePath); err != nil {
				fmt.Fprintf(os.Stderr, "   ❌ Failed to start service %s: %v\n", service.Name, err)
				fmt.Fprintf(os.Stderr, "   Hint: Run 'citadel logs %s' to see detailed output.\n", service.Name)
				os.Exit(1)
			}
		}
		fmt.Printf("   ✅ Service %s is up.\n", service.Name)
	}

	fmt.Println("\n🎉 All services are running.")

	// Try to sync SSH keys if configured
	syncSSHKeysIfConfigured(configDir)
}

// serviceIsKnown reports whether a service name is runnable: it is either an
// embedded/catalog service (in services.ServiceMap) or a module-installed
// service already tracked in the node manifest. The manifest is the source of
// truth for installed/module services, so checking both lets `citadel run
// <name>` accept services added via `citadel module install`.
func serviceIsKnown(serviceName string, manifest *CitadelManifest) bool {
	if _, ok := services.ServiceMap[serviceName]; ok {
		return true
	}
	return hasService(manifest, serviceName)
}

// knownServiceNames returns a sorted, deduplicated list of runnable service
// names: the embedded/catalog services plus any module-installed services from
// the manifest. Used for the "Available services" hint on an unknown name.
func knownServiceNames(manifest *CitadelManifest) []string {
	names := services.GetAvailableServices() // already sorted
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		seen[n] = struct{}{}
	}
	extra := make([]string, 0)
	for _, s := range manifest.Services {
		if _, ok := seen[s.Name]; ok {
			continue
		}
		seen[s.Name] = struct{}{}
		extra = append(extra, s.Name)
	}
	sort.Strings(extra)
	return append(names, extra...)
}

// runSingleService adds a service to the manifest (if needed) and starts it.
func runSingleService(serviceName string) {
	// Find or create manifest first: the manifest is the source of truth for
	// module-installed services, and it must be created on a fresh node so a
	// valid embedded service can be started on first run.
	manifest, configDir, err := findOrCreateManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to initialize configuration: %v\n", err)
		os.Exit(1)
	}

	// Validate service name against both the embedded catalog and the manifest
	// (which tracks module-installed services).
	if !serviceIsKnown(serviceName, manifest) {
		fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
		fmt.Printf("Available services: %s\n", strings.Join(knownServiceNames(manifest), ", "))
		os.Exit(1)
	}

	// Ensure compose file exists
	if err := ensureComposeFile(configDir, serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create compose file: %v\n", err)
		os.Exit(1)
	}

	// Strip GPU device reservations on non-Linux platforms
	composePath := filepath.Join(configDir, "services", serviceName+".yml")
	if !platform.IsLinux() {
		content, err := os.ReadFile(composePath)
		if err == nil {
			filtered, err := compose.StripGPUDevices(content)
			if err == nil {
				os.WriteFile(composePath, filtered, 0600)
				fmt.Println("   ℹ️  Running in CPU-only mode (GPU acceleration unavailable on this platform)")
			}
		}
	}

	// Add to manifest if not present
	if !hasService(manifest, serviceName) {
		if err := addServiceToManifest(configDir, serviceName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to update manifest: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Added '%s' to manifest\n", serviceName)
	}

	// Start the service
	fmt.Printf("--- 🚀 Starting service: %s ---\n", serviceName)

	if err := startService(serviceName, composePath); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to start service '%s': %v\n", serviceName, err)
		os.Exit(1)
	}

	if detachRun {
		fmt.Printf("\n✅ Service '%s' is running.\n", serviceName)
		fmt.Printf("   - To see logs, run: citadel logs %s -f\n", serviceName)
		fmt.Printf("   - To stop, run: citadel stop %s\n", serviceName)
	}
}

// restartAllServices restarts all services defined in the manifest.
func restartAllServices() {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	if len(manifest.Services) == 0 {
		fmt.Println("No services configured in manifest.")
		return
	}

	fmt.Printf("--- 🔄 Restarting %d service(s) ---\n", len(manifest.Services))

	for _, service := range manifest.Services {
		// Validate that compose file path stays within config directory (prevent path traversal)
		fullComposePath, err := platform.ValidatePathWithinDir(configDir, service.ComposeFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "   ❌ Invalid compose file path for %s: %v\n", service.Name, err)
			continue
		}
		fmt.Printf("🔄 Restarting service: %s\n", service.Name)

		composeCmd := exec.Command("docker", "compose", "-f", fullComposePath, "restart")
		output, err := composeCmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "   ❌ Failed to restart service %s: %s\n", service.Name, string(output))
		} else {
			fmt.Printf("   ✅ Service %s restarted.\n", service.Name)
		}
	}

	fmt.Println("\n🎉 All services have been restarted.")
}

// syncSSHKeysIfConfigured attempts to sync SSH keys if configuration exists.
// Silently does nothing if SSH sync is not configured.
func syncSSHKeysIfConfigured(configDir string) {
	config, err := nexus.LoadSSHSyncConfig(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️ SSH sync config error: %v\n", err)
		return
	}
	if config == nil {
		// Not configured, silently skip
		return
	}

	fmt.Println("   - Syncing SSH authorized keys...")
	if err := nexus.SyncAuthorizedKeys(*config); err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️ SSH key sync failed: %v\n", err)
	} else {
		fmt.Println("   ✅ SSH keys synchronized")
	}
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().BoolVarP(&detachRun, "detach", "d", true, "Run in detached mode (background).")
	runCmd.Flags().BoolVarP(&forceRecreate, "force", "f", false, "Force recreate containers without prompting.")
	runCmd.Flags().BoolVarP(&restartServices, "restart", "r", false, "Restart services instead of starting fresh.")
}
