// cmd/catalog.go
// Service catalog commands: browse, search, and install services from the catalog repository.
package cmd

import (
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

var catalogInstallConfigFlags []string

var catalogInstallCmd = &cobra.Command{
	Use:   "install <name>",
	Short: "Install a service from the catalog onto this node",
	Args:  cobra.ExactArgs(1),
	RunE:  runCatalogInstall,
}

func init() {
	svcCmd.AddCommand(catalogCmd)
	catalogCmd.AddCommand(catalogUpdateCmd)
	catalogCmd.AddCommand(catalogListCmd)
	catalogCmd.AddCommand(catalogSearchCmd)
	catalogCmd.AddCommand(catalogInfoCmd)
	catalogCmd.AddCommand(catalogInstallCmd)

	catalogInstallCmd.Flags().StringArrayVar(&catalogInstallConfigFlags, "set", nil,
		"Set a config value (e.g. --set MODEL=Qwen/Qwen3-8B)")
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
	bold.Fprintf(w, "SERVICE\tVERSION\tCATEGORY\tGPU\tSTATUS\tDESCRIPTION\n")

	for _, s := range reg.Services {
		statusStr := color.New(color.FgWhite).Sprint("available")
		if installed[s.Name] {
			statusStr = color.New(color.FgGreen).Sprint("installed")
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Version, s.Category, s.GPU, statusStr, truncate(s.Description, 50))
	}
	return nil
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
	bold.Fprintf(w, "SERVICE\tVERSION\tCATEGORY\tGPU\tSTATUS\tDESCRIPTION\n")

	for _, s := range results {
		statusStr := color.New(color.FgWhite).Sprint("available")
		if installed[s.Name] {
			statusStr = color.New(color.FgGreen).Sprint("installed")
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Version, s.Category, s.GPU, statusStr, truncate(s.Description, 50))
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

	fmt.Printf("Installing %s from catalog...\n", name)

	result, err := catalog.Install(name, servicesDir, overrides)
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
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

// truncate is defined in mcp.go and reused here.
