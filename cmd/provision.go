// cmd/provision.go
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/provision"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Manage provisioned resources (containers, VMs)",
	Long: `Create, destroy, list, and inspect provisioned resources on this node.

Resources are managed declaratively via specs. The provisioning system
handles the full lifecycle: create, status reconciliation, crash recovery,
and cleanup.

Examples:
  # List all resources
  citadel provision list

  # Create a container
  citadel provision create --name my-app --image nginx:latest --port 8080:80

  # Check status
  citadel provision status <id>

  # View logs
  citadel provision logs <id>

  # Destroy a resource
  citadel provision destroy <id>`,
}

// --- provision list ---

var provisionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all provisioned resources",
	Run:   runProvisionList,
}

func runProvisionList(_ *cobra.Command, _ []string) {
	mgr, err := newProvisionManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	resources := mgr.List()
	if len(resources) == 0 {
		fmt.Println("No provisioned resources.")
		return
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATUS\tIMAGE\tCONTAINER")
	for _, r := range resources {
		cid := r.ContainerID
		if len(cid) > 12 {
			cid = cid[:12]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(r.ID), r.Spec.Name, r.Spec.Type, r.Status, r.Spec.Image, cid)
	}
	tw.Flush()
}

// --- provision create ---

var (
	provisionCreateName    string
	provisionCreateImage   string
	provisionCreatePorts   []string
	provisionCreateEnv     []string
	provisionCreateVolumes []string
	provisionCreateCPUs    string
	provisionCreateMemory  int
	provisionCreateGPUs    string
	provisionCreateCommand []string
)

var provisionCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new provisioned resource",
	Long: `Create a Docker container from a spec.

Examples:
  citadel provision create --name my-app --image nginx:latest
  citadel provision create --name db --image postgres:16 --port 5432:5432 --env POSTGRES_PASSWORD=secret
  citadel provision create --name gpu-worker --image pytorch/pytorch --gpus all`,
	Run: runProvisionCreate,
}

func runProvisionCreate(_ *cobra.Command, _ []string) {
	if provisionCreateName == "" {
		fmt.Fprintln(os.Stderr, "Error: --name is required")
		os.Exit(1)
	}
	if provisionCreateImage == "" {
		fmt.Fprintln(os.Stderr, "Error: --image is required")
		os.Exit(1)
	}

	spec := &provision.ResourceSpec{
		Name:     provisionCreateName,
		Type:     provision.ResourceTypeDocker,
		Image:    provisionCreateImage,
		CPUs:     provisionCreateCPUs,
		MemoryMB: provisionCreateMemory,
		GPUs:     provisionCreateGPUs,
		Command:  provisionCreateCommand,
	}

	// Parse port mappings.
	for _, p := range provisionCreatePorts {
		pm, err := parsePortMapping(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid port mapping %q: %v\n", p, err)
			os.Exit(1)
		}
		spec.Ports = append(spec.Ports, pm)
	}

	// Parse environment variables.
	env := make(map[string]string)
	for _, e := range provisionCreateEnv {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "Error: invalid env var %q (expected KEY=VALUE)\n", e)
			os.Exit(1)
		}
		env[k] = v
	}
	if len(env) > 0 {
		spec.Env = env
	}

	// Parse volumes.
	for _, vol := range provisionCreateVolumes {
		vm, err := parseVolumeMount(vol)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid volume %q: %v\n", vol, err)
			os.Exit(1)
		}
		spec.Volumes = append(spec.Volumes, vm)
	}

	mgr, err := newProvisionManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	result, err := mgr.Create(context.Background(), spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if result.Reused {
		color.Yellow("Resource %q already exists (ID: %s)\n", spec.Name, shortID(result.Resource.ID))
	} else {
		color.Green("Resource %q created (ID: %s)\n", spec.Name, shortID(result.Resource.ID))
	}

	printResource(result.Resource)
}

// --- provision status ---

var provisionStatusCmd = &cobra.Command{
	Use:   "status <id>",
	Short: "Show the status of a provisioned resource",
	Args:  cobra.ExactArgs(1),
	Run:   runProvisionStatus,
}

func runProvisionStatus(_ *cobra.Command, args []string) {
	mgr, err := newProvisionManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	id := resolveResourceID(mgr, args[0])

	resource, err := mgr.Status(context.Background(), id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	printResource(resource)
}

// --- provision destroy ---

var provisionDestroyCmd = &cobra.Command{
	Use:   "destroy <id>",
	Short: "Destroy a provisioned resource",
	Args:  cobra.ExactArgs(1),
	Run:   runProvisionDestroy,
}

func runProvisionDestroy(_ *cobra.Command, args []string) {
	mgr, err := newProvisionManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	id := resolveResourceID(mgr, args[0])

	if err := mgr.Destroy(context.Background(), id); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	color.Green("Resource %s destroyed.\n", shortID(id))
}

// --- provision logs ---

var provisionLogsTail int

var provisionLogsCmd = &cobra.Command{
	Use:   "logs <id>",
	Short: "View logs from a provisioned resource",
	Args:  cobra.ExactArgs(1),
	Run:   runProvisionLogs,
}

func runProvisionLogs(_ *cobra.Command, args []string) {
	mgr, err := newProvisionManager()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	id := resolveResourceID(mgr, args[0])

	logs, err := mgr.Logs(context.Background(), id, provisionLogsTail)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Print(logs)
}

// --- Helpers ---

func newProvisionManager() (*provision.Manager, error) {
	configDir := platform.ConfigDir()

	store, err := provision.NewStore(configDir)
	if err != nil {
		return nil, fmt.Errorf("initializing resource store: %w", err)
	}

	docker, err := provision.NewDockerBackend()
	if err != nil {
		return nil, fmt.Errorf("initializing Docker backend: %w", err)
	}

	backends := map[provision.ResourceType]provision.Backend{
		provision.ResourceTypeDocker: docker,
	}

	mgr := provision.NewManager(store, backends)
	mgr.ReconcileAll(context.Background())

	return mgr, nil
}

func resolveResourceID(mgr *provision.Manager, input string) string {
	if r := mgr.Get(input); r != nil {
		return input
	}
	for _, r := range mgr.List() {
		if r.Spec.Name == input {
			return r.ID
		}
	}
	for _, r := range mgr.List() {
		if strings.HasPrefix(r.ID, input) {
			return r.ID
		}
	}
	return input
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func printResource(r *provision.Resource) {
	data, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(data))
}

func parsePortMapping(s string) (provision.PortMapping, error) {
	var pm provision.PortMapping
	proto := "tcp"
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		proto = s[idx+1:]
		s = s[:idx]
	}
	pm.Protocol = proto

	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return pm, fmt.Errorf("expected host:container format")
	}
	if _, err := fmt.Sscanf(parts[0], "%d", &pm.HostPort); err != nil {
		return pm, fmt.Errorf("invalid host port: %w", err)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &pm.ContainerPort); err != nil {
		return pm, fmt.Errorf("invalid container port: %w", err)
	}
	return pm, nil
}

func parseVolumeMount(s string) (provision.VolumeMount, error) {
	var vm provision.VolumeMount
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 {
		return vm, fmt.Errorf("expected host:container format")
	}
	vm.HostPath = parts[0]
	vm.ContainerPath = parts[1]
	if len(parts) == 3 && parts[2] == "ro" {
		vm.ReadOnly = true
	}
	return vm, nil
}

func init() {
	rootCmd.AddCommand(provisionCmd)
	provisionCmd.AddCommand(provisionListCmd)
	provisionCmd.AddCommand(provisionCreateCmd)
	provisionCmd.AddCommand(provisionStatusCmd)
	provisionCmd.AddCommand(provisionDestroyCmd)
	provisionCmd.AddCommand(provisionLogsCmd)

	provisionCreateCmd.Flags().StringVar(&provisionCreateName, "name", "", "Resource name (required)")
	provisionCreateCmd.Flags().StringVar(&provisionCreateImage, "image", "", "Container image (required)")
	provisionCreateCmd.Flags().StringSliceVarP(&provisionCreatePorts, "port", "p", nil, "Port mapping (host:container[/protocol])")
	provisionCreateCmd.Flags().StringSliceVarP(&provisionCreateEnv, "env", "e", nil, "Environment variable (KEY=VALUE)")
	provisionCreateCmd.Flags().StringSliceVarP(&provisionCreateVolumes, "volume", "v", nil, "Volume mount (host:container[:ro])")
	provisionCreateCmd.Flags().StringVar(&provisionCreateCPUs, "cpus", "", "CPU limit (e.g., 0.5)")
	provisionCreateCmd.Flags().IntVar(&provisionCreateMemory, "memory", 0, "Memory limit in MB")
	provisionCreateCmd.Flags().StringVar(&provisionCreateGPUs, "gpus", "", "GPU access (e.g., all, 1)")
	provisionCreateCmd.Flags().StringSliceVar(&provisionCreateCommand, "cmd", nil, "Override container command")

	provisionLogsCmd.Flags().IntVar(&provisionLogsTail, "tail", 100, "Number of log lines to show")
}
