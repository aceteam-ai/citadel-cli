// cmd/catalog.go
// Service catalog commands: browse, search, and install services from the catalog repository.
package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var catalogCmd = &cobra.Command{
	Use:   "catalog",
	Short: "Browse and install services from the catalog",
	Long: `Manage the service catalog — a curated repository of containerized services
that can be installed on your Citadel node.

Run 'citadel service catalog update' to download or refresh the catalog,
then use list, search, and info to discover services.`,
}

var catalogUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Download or refresh the local service catalog",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Println("Updating service catalog...")
		if err := catalog.Update(); err != nil {
			return fmt.Errorf("failed to update catalog: %w", err)
		}
		fmt.Println("Catalog updated successfully.")
		return nil
	},
}

var catalogListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available services in the catalog",
	RunE:  runCatalogList,
}

var catalogSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search for services by name, tag, or category",
	Args:  cobra.ExactArgs(1),
	RunE:  runCatalogSearch,
}

var catalogInfoCmd = &cobra.Command{
	Use:   "info <name>",
	Short: "Show full details about a catalog service",
	Args:  cobra.ExactArgs(1),
	RunE:  runCatalogInfo,
}

var (
	catalogInstallConfigFlags     []string
	catalogInstallAllowPrivileged bool
)

var catalogInstallCmd = &cobra.Command{
	Use:   "install <name>",
	Short: "Install a service from the catalog onto this node",
	Args:  cobra.ExactArgs(1),
	RunE:  runCatalogInstall,
}

var catalogAddName string

var catalogAddCmd = &cobra.Command{
	Use:   "add <git-url>",
	Short: "Register an additional community catalog source",
	Long: `Register an additional catalog source -- a git repository laid out like the
official catalog (a top-level registry.yaml and services/<name>/ subdirectories).
After adding a source, run 'citadel service catalog update' to fetch it.

The source name defaults to the repository name; override it with --name. The
built-in official source is always present and is named "default".`,
	Args: cobra.ExactArgs(1),
	RunE: runCatalogAdd,
}

var catalogRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Unregister a community catalog source",
	Args:  cobra.ExactArgs(1),
	RunE:  runCatalogRemove,
}

var catalogListSourcesCmd = &cobra.Command{
	Use:   "list-sources",
	Short: "List configured catalog sources",
	RunE:  runCatalogListSources,
}

func init() {
	svcCmd.AddCommand(catalogCmd)
	catalogCmd.AddCommand(catalogUpdateCmd)
	catalogCmd.AddCommand(catalogListCmd)
	catalogCmd.AddCommand(catalogSearchCmd)
	catalogCmd.AddCommand(catalogInfoCmd)
	catalogCmd.AddCommand(catalogInstallCmd)
	catalogCmd.AddCommand(catalogAddCmd)
	catalogCmd.AddCommand(catalogRemoveCmd)
	catalogCmd.AddCommand(catalogListSourcesCmd)

	catalogInstallCmd.Flags().StringArrayVar(&catalogInstallConfigFlags, "set", nil,
		"Set a config value (e.g. --set MODEL=Qwen/Qwen3-8B)")
	catalogInstallCmd.Flags().BoolVar(&catalogInstallAllowPrivileged, "allow-privileged", false,
		"Allow a community-source service whose compose requests privileged/root-equivalent access")
	catalogAddCmd.Flags().StringVar(&catalogAddName, "name", "",
		"Name for the source (defaults to the repository name)")
}

// runCatalogList prints all catalog services in a table with install status.
func runCatalogList(cmd *cobra.Command, args []string) error {
	reg, err := catalog.LoadRegistry()
	if err != nil {
		return err
	}

	if len(reg.Services) == 0 {
		fmt.Println("No services found in catalog.")
		return nil
	}

	// Determine which services are already installed by checking the node's services dir.
	installed := installedServiceSet()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	bold := color.New(color.Bold)
	bold.Fprintf(w, "SERVICE\tVERSION\tCATEGORY\tGPU\tSOURCE\tSTATUS\tDESCRIPTION\n")

	for _, s := range reg.Services {
		statusStr := color.New(color.FgWhite).Sprint("available")
		if installed[s.Name] {
			statusStr = color.New(color.FgGreen).Sprint("installed")
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Version, s.Category, s.GPU, sourceLabel(s.Source), statusStr, truncate(s.Description, 50))
	}
	return nil
}

// sourceLabel renders a registry entry's source name for table output, defaulting
// to the built-in default source name when unset (e.g. legacy single-source).
func sourceLabel(source string) string {
	if source == "" {
		return catalog.DefaultSourceName
	}
	return source
}

// runCatalogSearch prints services matching the query.
func runCatalogSearch(cmd *cobra.Command, args []string) error {
	results, err := catalog.Search(args[0])
	if err != nil {
		return err
	}

	if len(results) == 0 {
		fmt.Printf("No services matching '%s'.\n", args[0])
		return nil
	}

	installed := installedServiceSet()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	bold := color.New(color.Bold)
	bold.Fprintf(w, "SERVICE\tVERSION\tCATEGORY\tGPU\tSOURCE\tSTATUS\tDESCRIPTION\n")

	for _, s := range results {
		statusStr := color.New(color.FgWhite).Sprint("available")
		if installed[s.Name] {
			statusStr = color.New(color.FgGreen).Sprint("installed")
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Version, s.Category, s.GPU, sourceLabel(s.Source), statusStr, truncate(s.Description, 50))
	}
	return nil
}

// runCatalogInfo prints the full manifest details for a service.
func runCatalogInfo(cmd *cobra.Command, args []string) error {
	manifest, err := catalog.LoadServiceManifest(args[0])
	if err != nil {
		return err
	}

	bold := color.New(color.Bold)

	bold.Print("Name:        ")
	fmt.Println(manifest.Name)
	if src := catalogSourceOf(manifest.Name); src != "" {
		bold.Print("Source:      ")
		fmt.Println(src)
	}
	bold.Print("Version:     ")
	fmt.Println(manifest.Version)
	bold.Print("Description: ")
	fmt.Println(manifest.Description)
	bold.Print("Category:    ")
	fmt.Println(manifest.Category)
	if manifest.Author != "" {
		bold.Print("Author:      ")
		fmt.Println(manifest.Author)
	}
	if manifest.License != "" {
		bold.Print("License:     ")
		fmt.Println(manifest.License)
	}
	if manifest.Homepage != "" {
		bold.Print("Homepage:    ")
		fmt.Println(manifest.Homepage)
	}

	// Requirements
	fmt.Println()
	bold.Println("Requirements:")
	if manifest.Requires.GPU {
		fmt.Println("  GPU:       required")
	} else {
		fmt.Println("  GPU:       no")
	}
	if manifest.Requires.VRAMMinGB > 0 {
		fmt.Printf("  Min VRAM:  %.0f GB\n", manifest.Requires.VRAMMinGB)
	}
	if len(manifest.Requires.Arch) > 0 {
		fmt.Printf("  Arch:      %s\n", strings.Join(manifest.Requires.Arch, ", "))
	}

	// Ports
	if len(manifest.Ports) > 0 {
		fmt.Println()
		bold.Println("Ports:")
		for _, p := range manifest.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			desc := ""
			if p.Description != "" {
				desc = "  " + p.Description
			}
			fmt.Printf("  %d -> %d/%s%s\n", p.Host, p.Container, proto, desc)
		}
	}

	// Config
	if len(manifest.Config) > 0 {
		fmt.Println()
		bold.Println("Configuration:")

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, cv := range manifest.Config {
			def := ""
			if cv.Default != "" {
				def = fmt.Sprintf("[default: %s]", cv.Default)
			} else if cv.Required {
				def = color.New(color.FgRed).Sprint("[required]")
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\n", cv.Name, cv.Description, def)
		}
		w.Flush()
	}

	// Volumes
	if len(manifest.Volumes) > 0 {
		fmt.Println()
		bold.Println("Volumes:")
		for _, v := range manifest.Volumes {
			desc := ""
			if v.Description != "" {
				desc = "  " + v.Description
			}
			fmt.Printf("  %s -> %s%s\n", v.Host, v.Container, desc)
		}
	}

	// Tags
	if len(manifest.Tags) > 0 {
		fmt.Println()
		bold.Print("Tags: ")
		fmt.Println(strings.Join(manifest.Tags, ", "))
	}

	return nil
}

// runCatalogInstall installs a service from the catalog and registers it in the manifest.
func runCatalogInstall(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Parse --set flags into a map.
	overrides := make(map[string]string)
	for _, kv := range catalogInstallConfigFlags {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid --set value '%s': expected KEY=VALUE", kv)
		}
		overrides[parts[0]] = parts[1]
	}

	// Reject host-provisioned services (no compose.yml, e.g. the Windows-only
	// "wechat" microservice) before doing any work. Print provisioning guidance
	// instead of scaffolding node config and a misleading "Installing..." line.
	if !catalog.IsInstallable(name) {
		// Confirm the service actually exists; if not, surface the load error.
		if _, mErr := catalog.LoadServiceManifest(name); mErr != nil {
			return mErr
		}
		return printNotInstallableGuidance(name)
	}

	// Find or create the node manifest to get the services directory.
	manifest, configDir, err := findOrCreateManifest()
	if err != nil {
		return fmt.Errorf("failed to initialize configuration: %w", err)
	}

	// Check if already installed.
	if hasService(manifest, name) {
		fmt.Printf("Service '%s' is already in the node manifest.\n", name)
		return nil
	}

	servicesDir := filepath.Join(configDir, "services")

	// Trust level depends on the owning source. The built-in default source is
	// first-party (Tier 0) and installs as-is. A community source is untrusted
	// (Tier 2): the install runs through the least-privilege sandbox and the
	// un-bypassable privilege gate (overridable only with --allow-privileged),
	// mirroring `citadel module install` of an external git source.
	source := catalog.SourceOf(name)
	untrusted := !catalog.IsDefaultSource(source)

	if untrusted {
		fmt.Printf("Installing %s from community source '%s'...\n", name, source)
	} else {
		fmt.Printf("Installing %s from catalog...\n", name)
	}

	svcManifest, err := catalog.LoadServiceManifest(name)
	if err != nil {
		return err
	}
	composeSrc, _ := catalog.GetComposeFile(name)

	// allowPrivileged: trusted (default) sources are exempt from the gate, as
	// before. Community sources require an explicit --allow-privileged opt-in for
	// any privileged/root-equivalent compose directive.
	allowPrivileged := !untrusted || catalogInstallAllowPrivileged

	result, err := catalog.InstallFromManifest(svcManifest, composeSrc, servicesDir, overrides, true, allowPrivileged, untrusted)
	if err != nil {
		// Defense-in-depth: the up-front IsInstallable check above already
		// diverts host-provisioned services, but InstallFromManifest also guards.
		if errors.Is(err, catalog.ErrNotInstallable) {
			return printNotInstallableGuidance(name)
		}
		return fmt.Errorf("install failed: %w", err)
	}

	if result.Sandboxed {
		fmt.Printf("  Applied least-privilege sandbox: %s\n", result.SandboxOverridePath)
	}

	// Register in the node manifest using existing helpers.
	if err := addServiceToManifest(configDir, result.Name); err != nil {
		return fmt.Errorf("failed to update manifest: %w", err)
	}

	fmt.Printf("\nInstalled %s successfully.\n", result.Name)
	fmt.Printf("  Compose: %s\n", result.ComposeDestPath)
	if result.EnvDestPath != "" {
		fmt.Printf("  Config:  %s\n", result.EnvDestPath)
	}
	fmt.Printf("\nTo start the service:\n")
	fmt.Printf("  citadel run %s\n", result.Name)

	return nil
}

// printNotInstallableGuidance explains that a host-provisioned service (no
// compose.yml) cannot be installed by the CLI and points the operator at its
// provisioning steps and per-person enablement flow.
func printNotInstallableGuidance(name string) error {
	manifest, mErr := catalog.LoadServiceManifest(name)

	bold := color.New(color.Bold)
	fmt.Printf("\n'%s' is host-provisioned and is not installable via the catalog.\n", name)
	fmt.Println("It is catalogued for discoverability only (no compose.yml / not a container).")

	if mErr == nil && manifest.Homepage != "" {
		fmt.Println()
		bold.Print("Provision it from: ")
		fmt.Println(manifest.Homepage)
	}

	if name == "wechat" {
		fmt.Println()
		bold.Println("Per-person enablement flow:")
		fmt.Println("  1. Provision a Windows VM:  provision/bootstrap.ps1")
		fmt.Println("  2. QR login to WeChat (scan on PVE console / RDP, confirm on phone)")
		fmt.Println("  3. Start the service:       provision/start-service.ps1  (uvicorn :8000)")
		fmt.Println("  4. The Citadel worker on this node already relays to the VM over the LAN")
		fmt.Println("  5. Bind + route per person: wechat_connect(api_url, api_key, node_id)")
		fmt.Println("     (node_id is this node's Headscale numeric ID from terminal_list_nodes)")
	}

	fmt.Printf("\nSee 'citadel service catalog info %s' for ports, health check, and config.\n", name)
	return nil
}

// installedServiceSet returns a set of service names that are in the current node manifest.
func installedServiceSet() map[string]bool {
	installed := make(map[string]bool)
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return installed
	}
	for _, s := range manifest.Services {
		installed[s.Name] = true
	}
	return installed
}

// catalogSourceOf returns the name of the source that owns service `name` (per
// the merged registry's collision precedence), or "" if it can't be determined.
func catalogSourceOf(name string) string {
	reg, err := catalog.LoadRegistry()
	if err != nil {
		return ""
	}
	for _, e := range reg.Services {
		if e.Name == name {
			return sourceLabel(e.Source)
		}
	}
	return ""
}

// runCatalogAdd registers a new community catalog source.
func runCatalogAdd(cmd *cobra.Command, args []string) error {
	url := args[0]
	name := strings.TrimSpace(catalogAddName)
	if name == "" {
		name = catalog.DefaultSourceNameFromURL(url)
		if name == "" {
			return fmt.Errorf("could not derive a source name from %q; pass --name", url)
		}
	}

	if err := catalog.AddSource(name, url); err != nil {
		return err
	}

	fmt.Printf("Added catalog source '%s' (%s).\n", name, url)
	fmt.Println("Run 'citadel service catalog update' to fetch it.")
	return nil
}

// runCatalogRemove unregisters a community catalog source.
func runCatalogRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	if err := catalog.RemoveSource(name); err != nil {
		return err
	}
	fmt.Printf("Removed catalog source '%s'.\n", name)
	return nil
}

// runCatalogListSources lists every configured catalog source.
func runCatalogListSources(cmd *cobra.Command, args []string) error {
	sources, err := catalog.ListSources()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	bold := color.New(color.Bold)
	bold.Fprintf(w, "NAME\tTYPE\tURL\n")

	for _, s := range sources {
		typ := "community"
		if s.Name == catalog.DefaultSourceName {
			typ = "default"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, typ, s.URL)
	}
	return nil
}

// truncate is defined in mcp.go and reused here.
