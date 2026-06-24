// cmd/module.go
//
// `citadel module` installs and inspects Citadel service modules from any
// standardized git repo, not just the central catalog. A module repo
// self-describes via a top-level `citadel/` directory containing service.yaml +
// compose.yml (the compose `image:` points at that repo's own registry); the CLI
// never hardcodes a module's image.
//
// Source forms accepted by `<source>`:
//   - a catalog name (e.g. "vllm")           -> resolved from the central catalog
//   - "owner/repo" or "owner/repo@ref"        -> https://github.com/owner/repo.git
//   - a full git URL (https://… or git@…:…)   -> cloned directly
//
// This is a separate top-level group from `citadel service install` (the systemd
// service-install command) and from `citadel service catalog` (the central
// catalog browser).
//
// Note on private repos: cloning a private source repo requires this node to have
// git credentials (a GITHUB_TOKEN, an SSH key, or a configured git credential
// helper). Public repos work for everyone; private repos work only for nodes with
// credentials.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/tui/controlcenter"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	moduleInstallConfigFlags []string
	moduleInstallYes         bool
)

var moduleCmd = &cobra.Command{
	Use:   "module",
	Short: "Install service modules from any standardized git repo",
	Long: `Install and inspect Citadel service modules from any standardized git repo,
not just the central catalog.

A module repo self-describes via a top-level 'citadel/' directory containing
service.yaml + compose.yml, with the compose 'image:' pointing at that repo's own
container registry. The CLI never hardcodes a module's image.

Sources can be a catalog name (e.g. vllm), an 'owner/repo' shorthand
(optionally 'owner/repo@ref'), or a full git URL.

  citadel module install owner/repo            # install from GitHub
  citadel module install owner/repo@v1.2.0     # pin a tag/branch
  citadel module install https://git.example/m # full git URL
  citadel module install vllm                  # delegate to the central catalog

Private source repos require this node to have git credentials (a GITHUB_TOKEN,
an SSH key, or a configured git credential helper).`,
}

var moduleInstallCmd = &cobra.Command{
	Use:   "install <source>",
	Short: "Install a service module from a catalog name, owner/repo, or git URL",
	Args:  cobra.ExactArgs(1),
	RunE:  runModuleInstall,
}

var moduleListCmd = &cobra.Command{
	Use:   "list",
	Short: "List installed modules (services in the node manifest)",
	RunE:  runModuleList,
}

var moduleInfoCmd = &cobra.Command{
	Use:   "info <source>",
	Short: "Resolve a module source and print its manifest details",
	Args:  cobra.ExactArgs(1),
	RunE:  runModuleInfo,
}

func init() {
	rootCmd.AddCommand(moduleCmd)
	moduleCmd.AddCommand(moduleInstallCmd)
	moduleCmd.AddCommand(moduleListCmd)
	moduleCmd.AddCommand(moduleInfoCmd)

	moduleInstallCmd.Flags().StringArrayVar(&moduleInstallConfigFlags, "set", nil,
		"Set a config value (e.g. --set KEY=VALUE). Repeatable.")
	moduleInstallCmd.Flags().BoolVar(&moduleInstallYes, "yes", false,
		"Skip the confirmation prompt for external (non-catalog) sources.")
}

// parseSetFlags converts repeated --set KEY=VALUE flags into an overrides map.
func parseSetFlags(flags []string) (map[string]string, error) {
	overrides := make(map[string]string)
	for _, kv := range flags {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid --set value '%s': expected KEY=VALUE", kv)
		}
		overrides[parts[0]] = parts[1]
	}
	return overrides, nil
}

// runModuleInstall installs a module from any source: catalog name (delegated to
// the existing catalog flow) or an external git source resolved on the fly.
func runModuleInstall(cmd *cobra.Command, args []string) error {
	src, err := catalog.ParseSource(args[0])
	if err != nil {
		return err
	}

	// Catalog names delegate to the existing catalog install flow so that
	// host-provisioned services and wechat-style guidance do not diverge.
	if src.Kind == catalog.KindCatalog {
		catalogInstallConfigFlags = moduleInstallConfigFlags
		return runCatalogInstall(cmd, []string{src.Name})
	}

	overrides, err := parseSetFlags(moduleInstallConfigFlags)
	if err != nil {
		return err
	}

	// Resolve the external source: clone/update the repo and load its manifest.
	fmt.Printf("Resolving module source %s ...\n", color.CyanString(src.Raw))
	manifest, composeSrc, err := catalog.ResolveSource(src)
	if err != nil {
		return err
	}

	// Print a summary so the operator can confirm what will be installed.
	image := composeImage(composeSrc)
	printModuleSummary(src, manifest, composeSrc, image, overrides)

	if !moduleInstallYes {
		fmt.Print("\nProceed with install? [y/N]: ")
		if !confirmYes() {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// Find or create the node manifest to get the services directory.
	nodeManifest, configDir, err := findOrCreateManifest()
	if err != nil {
		return fmt.Errorf("failed to initialize configuration: %w", err)
	}
	if hasService(nodeManifest, manifest.Name) {
		fmt.Printf("Module '%s' is already in the node manifest.\n", manifest.Name)
		return nil
	}

	servicesDir := filepath.Join(configDir, "services")

	fmt.Printf("\nInstalling %s ...\n", manifest.Name)
	result, err := catalog.InstallFromManifest(manifest, composeSrc, servicesDir, overrides, true)
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}

	if err := addServiceToManifest(configDir, result.Name); err != nil {
		return fmt.Errorf("failed to update manifest: %w", err)
	}

	fmt.Printf("\nInstalled %s successfully.\n", result.Name)
	fmt.Printf("  Compose: %s\n", result.ComposeDestPath)
	if result.EnvDestPath != "" {
		fmt.Printf("  Config:  %s\n", result.EnvDestPath)
	}
	fmt.Printf("\nTo start the module:\n")
	fmt.Printf("  citadel run %s\n", result.Name)
	return nil
}

// runModuleList prints the modules currently registered in the node manifest.
func runModuleList(cmd *cobra.Command, args []string) error {
	manifest, _, err := findAndReadManifest()
	if err != nil {
		fmt.Println("No node manifest found. Install a module with 'citadel module install <source>'.")
		return nil
	}
	if len(manifest.Services) == 0 {
		fmt.Println("No modules installed.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	bold := color.New(color.Bold)
	bold.Fprintf(w, "MODULE\tCOMPOSE\n")
	for _, s := range manifest.Services {
		fmt.Fprintf(w, "%s\t%s\n", s.Name, s.ComposeFile)
	}
	return nil
}

// runModuleInfo resolves a module source and prints its manifest details.
func runModuleInfo(cmd *cobra.Command, args []string) error {
	src, err := catalog.ParseSource(args[0])
	if err != nil {
		return err
	}

	// Catalog names reuse the existing catalog info renderer.
	if src.Kind == catalog.KindCatalog {
		return runCatalogInfo(cmd, []string{src.Name})
	}

	fmt.Printf("Resolving module source %s ...\n\n", color.CyanString(src.Raw))
	manifest, composeSrc, err := catalog.ResolveSource(src)
	if err != nil {
		return err
	}
	renderManifest(manifest, src, composeSrc, composeImage(composeSrc))
	return nil
}

// printModuleSummary prints a concise pre-install summary for an external module.
func printModuleSummary(src catalog.Source, manifest *catalog.ServiceManifest, composePath, image string, overrides map[string]string) {
	bold := color.New(color.Bold)
	fmt.Println()
	bold.Println("Module to install:")
	fmt.Printf("  Source:  %s\n", src.Raw)
	fmt.Printf("  Name:    %s\n", manifest.Name)
	if manifest.Version != "" {
		fmt.Printf("  Version: %s\n", manifest.Version)
	}
	if image != "" {
		fmt.Printf("  Image:   %s\n", image)
	}
	if len(manifest.Ports) > 0 {
		var ports []string
		for _, p := range manifest.Ports {
			ports = append(ports, fmt.Sprintf("%d->%d", p.Host, p.Container))
		}
		fmt.Printf("  Ports:   %s\n", strings.Join(ports, ", "))
	}
	// Required config (missing an override) -- highlight so the operator knows
	// what must be supplied via --set.
	var missing []string
	for _, cv := range manifest.Config {
		if cv.Required && cv.Default == "" {
			if _, ok := overrides[cv.Name]; !ok {
				missing = append(missing, cv.Name)
			}
		}
	}
	if len(missing) > 0 {
		fmt.Printf("  %s %s\n", color.YellowString("Required config (provide via --set):"), strings.Join(missing, ", "))
	}
}

// renderManifest prints full manifest details, mirroring runCatalogInfo's style,
// with the resolved source + image added at the top.
func renderManifest(manifest *catalog.ServiceManifest, src catalog.Source, composePath, image string) {
	bold := color.New(color.Bold)

	bold.Print("Source:      ")
	fmt.Println(src.Raw)
	if image != "" {
		bold.Print("Image:       ")
		fmt.Println(image)
	}
	bold.Print("Name:        ")
	fmt.Println(manifest.Name)
	bold.Print("Version:     ")
	fmt.Println(manifest.Version)
	bold.Print("Description: ")
	fmt.Println(manifest.Description)
	if manifest.Category != "" {
		bold.Print("Category:    ")
		fmt.Println(manifest.Category)
	}
	if manifest.Author != "" {
		bold.Print("Author:      ")
		fmt.Println(manifest.Author)
	}
	if manifest.Homepage != "" {
		bold.Print("Homepage:    ")
		fmt.Println(manifest.Homepage)
	}

	if manifest.Requires.GPU || manifest.Requires.VRAMMinGB > 0 || len(manifest.Requires.Arch) > 0 {
		fmt.Println()
		bold.Println("Requirements:")
		if manifest.Requires.GPU {
			fmt.Println("  GPU:       required")
		}
		if manifest.Requires.VRAMMinGB > 0 {
			fmt.Printf("  Min VRAM:  %.0f GB\n", manifest.Requires.VRAMMinGB)
		}
		if len(manifest.Requires.Arch) > 0 {
			fmt.Printf("  Arch:      %s\n", strings.Join(manifest.Requires.Arch, ", "))
		}
	}

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

	if len(manifest.Tags) > 0 {
		fmt.Println()
		bold.Print("Tags: ")
		fmt.Println(strings.Join(manifest.Tags, ", "))
	}
}

// composeImage extracts the first `image:` value from a compose file, for the
// install summary. Best-effort: returns "" if it cannot be determined. The CLI
// never hardcodes or rewrites the image -- this is display-only.
func composeImage(composePath string) string {
	data, err := os.ReadFile(composePath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "image:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

// confirmYes reads a single line from stdin and reports whether it is an
// affirmative ("y"/"yes", case-insensitive).
func confirmYes() bool {
	var resp string
	if _, err := fmt.Scanln(&resp); err != nil {
		return false
	}
	resp = strings.ToLower(strings.TrimSpace(resp))
	return resp == "y" || resp == "yes"
}

// buildModuleInstallCallbacks wires the control center's module-install page to
// the same source-resolution + install logic the CLI uses. The TUI collects all
// required config up front and passes it as overrides, so the non-interactive
// installer path is used (no stdin reads). All hooks are best-effort.
func buildModuleInstallCallbacks() controlcenter.ModuleInstallCallbacks {
	return controlcenter.ModuleInstallCallbacks{
		Resolve: func(source string) (controlcenter.ModuleResolveResult, error) {
			var out controlcenter.ModuleResolveResult
			src, err := catalog.ParseSource(source)
			if err != nil {
				return out, err
			}

			var manifest *catalog.ServiceManifest
			var composeSrc string
			if src.Kind == catalog.KindCatalog {
				manifest, err = catalog.LoadServiceManifest(src.Name)
				if err != nil {
					return out, err
				}
				composeSrc, _ = catalog.GetComposeFile(src.Name)
			} else {
				manifest, composeSrc, err = catalog.ResolveSource(src)
				if err != nil {
					return out, err
				}
			}

			out.Name = manifest.Name
			out.Image = composeImage(composeSrc)
			for _, cv := range manifest.Config {
				out.Config = append(out.Config, controlcenter.ConfigField{
					Name:        cv.Name,
					Description: cv.Description,
					Default:     cv.Default,
					Required:    cv.Required,
				})
			}
			return out, nil
		},
		Install: func(source string, overrides map[string]string) (string, error) {
			src, err := catalog.ParseSource(source)
			if err != nil {
				return "", err
			}

			var manifest *catalog.ServiceManifest
			var composeSrc string
			if src.Kind == catalog.KindCatalog {
				manifest, err = catalog.LoadServiceManifest(src.Name)
				if err != nil {
					return "", err
				}
				composeSrc, _ = catalog.GetComposeFile(src.Name)
			} else {
				manifest, composeSrc, err = catalog.ResolveSource(src)
				if err != nil {
					return "", err
				}
			}

			_, configDir, err := findOrCreateManifest()
			if err != nil {
				return "", err
			}
			servicesDir := filepath.Join(configDir, "services")

			// interactive=false: the TUI supplies all required config as
			// overrides, so a missing required var is an error, never a stdin read.
			result, err := catalog.InstallFromManifest(manifest, composeSrc, servicesDir, overrides, false)
			if err != nil {
				return "", err
			}
			if err := addServiceToManifest(configDir, result.Name); err != nil {
				return "", fmt.Errorf("failed to update manifest: %w", err)
			}
			return result.Name, nil
		},
	}
}
