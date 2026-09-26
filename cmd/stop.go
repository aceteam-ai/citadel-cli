// cmd/stop.go
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	internalServices "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
)

var (
	removeContainer bool
	forceStop       bool
	dryRunStop      bool
)

// stopCmd represents the stop command
var stopCmd = &cobra.Command{
	Use:   "stop [service]",
	Short: "Stop services (all if no service specified, or a specific one)",
	Long: fmt.Sprintf(`Stops running services.

When a service name is provided, that specific service is stopped.
When no service is specified, all services in the manifest are stopped.

Available services: %s`, strings.Join(services.GetAvailableServices(), ", ")),
	Example: `  # Stop a specific service
  citadel stop vllm

  # Stop all services in the manifest
  citadel stop

  # Stop and remove the container and volumes
  citadel stop ollama --rm

  # Skip confirmation prompts (for scripts)
  citadel stop --force

  # Preview what would be stopped
  citadel stop --dry-run`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) == 0 {
			// No service specified - stop all manifest services
			stopAllServices()
			return
		}

		// Stop specific service
		serviceName := args[0]
		stopSingleService(serviceName)
	},
}

// confirmPrompt asks the user a yes/no question. Returns true if confirmed.
// defaultYes controls the default when the user presses Enter without typing.
// Skips the prompt and returns true if --force is set.
func confirmPrompt(question string, defaultYes bool) bool {
	if forceStop {
		return true
	}

	hint := "(y/N)"
	if defaultYes {
		hint = "(Y/n)"
	}
	fmt.Printf("%s %s ", question, hint)

	var response string
	fmt.Scanln(&response)
	response = strings.TrimSpace(strings.ToLower(response))

	if response == "" {
		return defaultYes
	}
	return response == "y" || response == "yes"
}

// stopAllServices stops all services defined in the manifest.
func stopAllServices() {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	if len(manifest.Services) == 0 {
		fmt.Println("No services configured in manifest.")
		return
	}

	// List services that will be affected
	serviceNames := make([]string, len(manifest.Services))
	for i, s := range manifest.Services {
		serviceNames[i] = s.Name
	}

	if dryRunStop {
		fmt.Printf("Would stop %d service(s): %s\n", len(manifest.Services), strings.Join(serviceNames, ", "))
		if removeContainer {
			fmt.Println("Would also remove containers and volumes.")
		}
		return
	}

	// Confirm before stopping all services
	if removeContainer {
		if !confirmPrompt(fmt.Sprintf("Remove %d service(s) and their volumes (%s)?", len(manifest.Services), strings.Join(serviceNames, ", ")), false) {
			fmt.Println("Aborted.")
			return
		}
	} else {
		if !confirmPrompt(fmt.Sprintf("Stop %d service(s) (%s)?", len(manifest.Services), strings.Join(serviceNames, ", ")), true) {
			fmt.Println("Aborted.")
			return
		}
	}

	fmt.Printf("--- 🛑 Stopping %d service(s) ---\n", len(manifest.Services))

	// Process in reverse order for graceful shutdown
	for i := len(manifest.Services) - 1; i >= 0; i-- {
		service := manifest.Services[i]
		fullComposePath := filepath.Join(configDir, service.ComposeFile)
		fmt.Printf("🔻 Stopping service: %s\n", service.Name)

		// Mark durably stopped FIRST (mirrors liveModuleOps.Stop, #528): the stop
		// must survive a `citadel work` restart / reboot, whose boot paths skip
		// services with desired_status: stopped. `citadel run [service]` clears
		// the marker again.
		if err := setServiceDesiredStatus(configDir, service.Name, "stopped"); err != nil {
			fmt.Fprintf(os.Stderr, "   ⚠️  Could not record stopped state for %s: %v\n", service.Name, err)
		}

		if err := stopServiceByCompose(fullComposePath, removeContainer); err != nil {
			fmt.Fprintf(os.Stderr, "   ❌ Failed to stop service %s: %v\n", service.Name, err)
		} else {
			fmt.Printf("   ✅ Service %s is stopped.\n", service.Name)
		}
		// Transitional (#528): also remove containers a pre-fix start left under
		// the legacy "citadel-<name>" compose project, invisible to the no-`-p`
		// down above.
		removeLegacyCitadelProject(service.Name)
	}

	fmt.Println("\n🎉 All services stopped.")
	fmt.Println("   Services stay stopped across restarts. Use 'citadel run <service>' to start one again.")
}

// stopSingleService stops a specific service.
func stopSingleService(serviceName string) {
	// Validate service name
	if _, ok := services.ServiceMap[serviceName]; !ok {
		fmt.Fprintf(os.Stderr, "❌ Unknown service '%s'.\n", serviceName)
		fmt.Printf("Available services: %s\n", strings.Join(services.GetAvailableServices(), ", "))
		os.Exit(1)
	}

	if dryRunStop {
		fmt.Printf("Would stop service: %s\n", serviceName)
		if removeContainer {
			fmt.Println("Would also remove container and volumes.")
		}
		return
	}

	// Confirm before removing volumes (data-destructive)
	if removeContainer {
		if !confirmPrompt(fmt.Sprintf("Remove service '%s' and its volumes?", serviceName), false) {
			fmt.Println("Aborted.")
			return
		}
	}

	// Try to find the service in the manifest
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// If no manifest, try to stop by container name directly
		fmt.Printf("--- 🛑 Stopping service: %s ---\n", serviceName)
		// A native engine (e.g. a host-managed ollama) must NOT fall into the
		// container path, which no-ops for a native process and prints a false
		// success (#1144). Resolve the kind by auto-detection here.
		if determineServiceType(Service{Name: serviceName}) == internalServices.ServiceTypeNative {
			stopNativeAndReport(serviceName)
			return
		}
		if err := stopServiceByContainer(serviceName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Service '%s' stopped.\n", serviceName)
		return
	}

	// Find service in manifest
	var composePath, svcType string
	foundInManifest := false
	for _, s := range manifest.Services {
		if s.Name == serviceName {
			composePath = filepath.Join(configDir, s.ComposeFile)
			svcType = s.Type
			foundInManifest = true
			break
		}
	}

	fmt.Printf("--- 🛑 Stopping service: %s ---\n", serviceName)

	// Native branch (#1144): a native engine's compose file is empty, so the
	// compose path below would run `docker compose down` against the config dir
	// (a no-op with no container) and print a false success. Route native
	// services through the process-based stop, which reports a host-systemd-owned
	// engine as guidance (ErrNativeExternallyManaged) rather than a false stop.
	// Honor an explicit manifest `type`; otherwise auto-detect.
	if determineServiceType(Service{Name: serviceName, Type: svcType}) == internalServices.ServiceTypeNative {
		// Record durable stopped intent first, exactly as the compose path does
		// (mirrors liveModuleOps.Stop, #528), but only for a service actually in
		// the manifest -- so a citadel-managed native engine stays stopped across
		// a `citadel work` restart / reboot. Harmless for a host-managed engine
		// (citadel never starts it either way); skipped when not in the manifest,
		// matching the container fallback which sets no marker.
		if foundInManifest {
			if err := setServiceDesiredStatus(configDir, serviceName, "stopped"); err != nil {
				fmt.Fprintf(os.Stderr, "⚠️  Could not record stopped state for %s: %v\n", serviceName, err)
			}
		}
		stopNativeAndReport(serviceName)
		return
	}

	if composePath != "" {
		// Mark durably stopped FIRST (mirrors liveModuleOps.Stop, #528): the stop
		// must survive a `citadel work` restart / reboot, whose boot paths skip
		// services with desired_status: stopped. `citadel run <service>` clears
		// the marker again.
		if err := setServiceDesiredStatus(configDir, serviceName, "stopped"); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Could not record stopped state for %s: %v\n", serviceName, err)
		}
		// Use docker compose down if we have the compose file
		if err := stopServiceByCompose(composePath, removeContainer); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to stop service '%s': %v\n", serviceName, err)
			os.Exit(1)
		}
		// Transitional (#528): also remove containers a pre-fix start left under
		// the legacy "citadel-<name>" compose project, invisible to the no-`-p`
		// down above.
		removeLegacyCitadelProject(serviceName)
	} else {
		// Fallback to direct container stop
		if err := stopServiceByContainer(serviceName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Printf("✅ Service '%s' stopped.\n", serviceName)
}

// stopNativeServiceFn is the CLI-side seam over services.StopNativeService so
// stopNativeAndReport is exercisable without a live process or /proc read.
var stopNativeServiceFn = internalServices.StopNativeService

// stopNativeAndReport runs the native-engine stop for `citadel stop` and prints
// the operator-facing result. A host-systemd-owned engine (#1144) yields
// guidance, not the compose path's false success.
func stopNativeAndReport(serviceName string) {
	// Deliberately the PROCESS check, not the serving probe (#649/#677): a wedged
	// engine that answers nothing is still a live process citadel should stop.
	if !internalServices.IsNativeServiceRunning(serviceName) {
		fmt.Printf("✅ Service '%s' is not running.\n", serviceName)
		return
	}
	line, _, failed := nativeStopOutcome(serviceName, stopNativeServiceFn(serviceName))
	if failed {
		fmt.Fprintln(os.Stderr, line)
		os.Exit(1)
	}
	fmt.Println(line)
}

// nativeStopOutcome maps a StopNativeService result to the CLI's operator-facing
// line. Pure and seam-free so it is unit-testable without a live process,
// manifest, or /proc read: a nil error is a real stop, an
// ErrNativeExternallyManaged is honest guidance (not a failure, not a false
// success), and any other error is a failure.
func nativeStopOutcome(serviceName string, err error) (line string, stopped bool, failed bool) {
	var extMgd *internalServices.ErrNativeExternallyManaged
	switch {
	case err == nil:
		return fmt.Sprintf("✅ Service '%s' stopped.", serviceName), true, false
	case errors.As(err, &extMgd):
		return internalServices.ExternallyManagedGuidance(serviceName, extMgd.Unit), false, false
	default:
		return fmt.Sprintf("❌ Failed to stop service '%s': %v", serviceName, err), false, true
	}
}

// stopComposeArgs builds the FULL compose args for `... down`, including the
// leading "compose" selector and the --node-dir project-scoping
// (composeArgsWithProject, citadel#856). stopServiceByCompose strips that
// leading "compose" and routes the rest through rt.ComposeCommand, which re-adds
// the resolved runtime's own front-end prefix -- byte-identical to the prior
// hardcoded `docker compose ...` on a docker node. Pure and separated from
// stopServiceByCompose so the argv contract is unit-testable without invoking
// docker (see TestStopComposeArgs*).
//
// The sibling env file (<name>.env next to <name>.yml) is passed on `down` too,
// mirroring the SERVICE_STOP path (internal/jobs/service_handler.go's down args)
// -- these two paths were divergent, and the missing --env-file here is a real
// bug: a compose file whose interpolation hard-requires a config var (the
// WhatsApp bridge's ${ADMIN_API_KEY:?}) fails to even PARSE on `down`, so an
// uninstall/stop of it retries forever (citadel#624). --env-file is a GLOBAL
// compose flag, so it must precede the `down` subcommand. compose.EnvFileArgs
// returns nil when no <name>.env exists, so this is a byte-identical no-op for
// every service without a sibling env file (the #528 default is preserved).
func stopComposeArgs(composePath string, remove bool) []string {
	fileArgs := []string{"-f", composePath}
	fileArgs = append(fileArgs, compose.EnvFileArgs(composePath)...)
	fileArgs = append(fileArgs, "down")
	if remove {
		fileArgs = append(fileArgs, "-v") // Also remove volumes
	}
	return append([]string{"compose"}, composeArgsWithProject(fileArgs)...)
}

// stopServiceByCompose stops a service using docker compose down.
func stopServiceByCompose(composePath string, remove bool) error {
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		return fmt.Errorf("compose file '%s' not found", composePath)
	}

	// stopComposeArgs already applies composeArgsWithProject (#856) and the
	// sibling --env-file (#624) and prepends "compose"; strip that leading
	// "compose" and let rt.ComposeCommand re-add the runtime's own front-end
	// prefix (docker/`podman compose`/podman-compose). Byte-identical to the
	// prior exec.Command("docker", ...) on a docker node.
	rt := catalog.SelectContainerRuntime()
	cmd := rt.ComposeCommand(stopComposeArgs(composePath, remove)[1:]...)
	// Inject CITADEL_WORKSPACE + host-port vars so compose files guarded with
	// ${VAR:?...} (transcribe/meeting workspace mount, #525) interpolate.
	cmd.Env = composeEnv()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose down failed:\n%s\n   Hint: Is Docker running? Check with 'docker info'", strings.TrimSpace(string(output)))
	}
	return nil
}

// stopServiceByContainer stops a service by its container name directly.
//
// Refuses outright under --node-dir/CITADEL_NODE_DIR (citadel#856 review):
// this fallback runs when serviceName is NOT found in the resolved manifest,
// and it operates on the bare GLOBAL container name via `docker
// inspect`/`stop`/`rm` -- none of which accept a compose-project scope the
// way `docker compose ... -p <project>` does, so composeArgsWithProject
// cannot protect this path. Under an override, "citadel-<serviceName>" may be
// a DIFFERENT node's real, running container on this same Docker daemon;
// silently stopping (or, with --rm, removing) it by name is exactly the
// incident class --node-dir exists to prevent. Refusing here is safe in both
// directions: the common override use case (a service intentionally NOT in
// the override's manifest) has nothing to stop anyway, and the dangerous case
// (a name collision with a real node) is refused rather than acted on.
func stopServiceByContainer(serviceName string) error {
	if composeProjectOverride() != "" {
		return fmt.Errorf(
			"refusing to stop %q: it is not in the --node-dir/CITADEL_NODE_DIR override's manifest, and "+
				"this fallback would stop a container by its GLOBAL name (citadel-%s) with no way to scope "+
				"that to the override -- it could be a DIFFERENT node's real container on this same Docker "+
				"daemon. Add %q to the override's citadel.yaml, or drop --node-dir to target the default node.",
			serviceName, serviceName, serviceName)
	}

	containerName := fmt.Sprintf("citadel-%s", serviceName)
	rt := catalog.SelectContainerRuntime()

	// Check if container exists
	inspectCmd := rt.EngineCommand("inspect", "--format", "{{.State.Status}}", containerName)
	output, err := inspectCmd.Output()
	if err != nil {
		return fmt.Errorf("container '%s' not found. Run 'citadel status' to see running services", containerName)
	}

	status := strings.TrimSpace(string(output))
	if status != "running" {
		fmt.Printf("   ℹ️  Container '%s' is not running (status: %s).\n", containerName, status)
		if removeContainer {
			return removeContainerByName(containerName)
		}
		return nil
	}

	// Stop the container
	stopCmd := rt.EngineCommand("stop", containerName)
	stopCmd.Stdout = os.Stdout
	stopCmd.Stderr = os.Stderr
	if err := stopCmd.Run(); err != nil {
		return fmt.Errorf("failed to stop container")
	}

	if removeContainer {
		return removeContainerByName(containerName)
	}
	return nil
}

// removeContainerByName removes a container by name.
func removeContainerByName(containerName string) error {
	fmt.Printf("--- Removing container '%s' ---\n", containerName)
	rmCmd := catalog.SelectContainerRuntime().EngineCommand("rm", containerName)
	rmCmd.Stdout = os.Stdout
	rmCmd.Stderr = os.Stderr
	if err := rmCmd.Run(); err != nil {
		return fmt.Errorf("failed to remove container")
	}
	fmt.Printf("✅ Container '%s' removed.\n", containerName)
	return nil
}

func init() {
	rootCmd.AddCommand(stopCmd)
	stopCmd.Flags().BoolVar(&removeContainer, "rm", false, "Remove the container/volumes after stopping.")
	stopCmd.Flags().BoolVarP(&forceStop, "force", "f", false, "Skip confirmation prompts.")
	stopCmd.Flags().BoolVar(&dryRunStop, "dry-run", false, "Show what would be stopped without doing it.")
}
