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
	moduleAllowPrivileged    bool
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
		"Skip the ordinary confirmation prompt (does NOT bypass the privileged-compose gate).")
	moduleInstallCmd.Flags().BoolVar(&moduleAllowPrivileged, "allow-privileged", false,
		"Allow installing a module whose compose requests privileged/root-equivalent access (required when Critical risks are present).")
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
	resolved, err := catalog.ResolveSource(src)
	if err != nil {
		return err
	}
	manifest := resolved.Manifest

	// Forward-compat: warn (don't fail) on a newer manifest schema.
	if w := catalog.SchemaWarning(manifest); w != "" {
		fmt.Println(color.YellowString("  Warning: " + w))
	}

	// Print a summary (incl. resolved commit + image) so the operator can confirm
	// what will be installed -- a real TOFU/provenance prompt.
	printModuleSummary(src, resolved, overrides)

	// Trust + risk surface: warn that this runs an arbitrary container with
	// root-equivalent access on this node (unless the source is trusted), and
	// list the compose risk findings.
	trusted := catalog.IsTrusted(src)
	risks := scanResolvedRisks(resolved)
	printTrustAndRisks(src, trusted, risks)

	// Hard gate: a Critical finding without --allow-privileged refuses the
	// install BEFORE any confirmation (and even under --yes). This mirrors the
	// shared-core guard in InstallFromManifest so the user sees a clear message
	// here. Trust does NOT relax the privilege gate.
	if crit := criticalDirectiveNames(risks); len(crit) > 0 && !moduleAllowPrivileged {
		return fmt.Errorf("refusing to install '%s': compose requests privileged/root-equivalent access (%s).\n"+
			"   This would grant the module Docker-level (host root) access on this node.\n"+
			"   If you trust this source, re-run with --allow-privileged to override.",
			manifest.Name, strings.Join(crit, ", "))
	}

	// Signature gate: if the matched trust entry is a verified publisher that
	// requires a signature, verify the image (by digest) with cosign BEFORE
	// install. This is a hard gate -- not bypassable by --yes -- and a no-op when
	// no signature is required (unsigned community modules still install under the
	// risk/consent gate above). Build the same lock images we'd record so we
	// verify exactly the digests we pin.
	lockImages := catalog.BuildLockImages(resolved.Images)
	verifyResult, err := catalog.VerifyModule(src, lockImages)
	if err != nil {
		return err
	}
	if verifyResult.Verified {
		fmt.Printf("  %s %s\n", color.GreenString("✓ signature verified"), color.New(color.Faint).Sprint(verifyResult.Image))
		lockImages = markLockImagesVerified(lockImages)
	}

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
	result, err := catalog.InstallFromManifest(manifest, resolved.ComposePath, servicesDir, overrides, true, moduleAllowPrivileged)
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}

	// Register in the node manifest, merging the module's declared routing tags
	// so a third-party engine becomes routable without a CLI change.
	if err := addServiceToManifestWithTags(configDir, result.Name, manifest.NodeTags); err != nil {
		return fmt.Errorf("failed to update manifest: %w", err)
	}

	// Record provenance into the lockfile (best-effort: never fail the install),
	// carrying the verified-signature flag computed above.
	recordModuleLock(src, resolved, lockImages)

	fmt.Printf("\nInstalled %s successfully.\n", result.Name)
	fmt.Printf("  Compose: %s\n", result.ComposeDestPath)
	if result.EnvDestPath != "" {
		fmt.Printf("  Config:  %s\n", result.EnvDestPath)
	}
	if len(manifest.NodeTags) > 0 {
		fmt.Printf("  Routing tags: %s\n", strings.Join(manifest.NodeTags, ", "))
	}
	fmt.Printf("\nTo start the module:\n")
	fmt.Printf("  citadel run %s\n", result.Name)
	return nil
}

// recordModuleLock upserts a provenance entry for a freshly resolved+installed
// module into modules.lock. Best-effort: any failure is reported to stderr but
// does not fail the install. If images is nil it is built from the resolved
// source; callers that already verified signatures pass the verified-flagged
// images so the result is recorded.
func recordModuleLock(src catalog.Source, resolved *catalog.ResolvedModule, images []catalog.LockImage) {
	if images == nil {
		images = catalog.BuildLockImages(resolved.Images)
	}
	entry := catalog.LockEntry{
		Name:   resolved.Manifest.Name,
		Source: src.Raw,
		Ref:    src.Ref,
		Commit: resolved.Commit,
		Images: images,
	}
	if err := catalog.UpsertLockEntry(entry); err != nil {
		fmt.Fprintf(os.Stderr, "  Note: could not record provenance in modules.lock: %v\n", err)
	}
}

// markLockImagesVerified returns a copy of images with Verified set on each entry
// (used after a successful signature verification).
func markLockImagesVerified(images []catalog.LockImage) []catalog.LockImage {
	out := make([]catalog.LockImage, len(images))
	for i, im := range images {
		im.Verified = true
		out[i] = im
	}
	return out
}

// runModuleList prints the modules registered in the node manifest, merged with
// provenance (source@commit, image[@digest]) from the lockfile when available.
// Services without a lockfile entry (catalog/embedded) are still shown.
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

	lf, _ := catalog.LoadLockfile() // best-effort; nil-safe below

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	bold := color.New(color.Bold)
	bold.Fprintf(w, "MODULE\tSOURCE\tIMAGE\n")
	for _, s := range manifest.Services {
		source := color.New(color.FgWhite).Sprint("catalog/embedded")
		image := "-"
		if lf != nil {
			if e, ok := lf.LookupLock(s.Name); ok {
				source = e.Source
				if e.Commit != "" {
					source = fmt.Sprintf("%s@%s", e.Source, shortCommit(e.Commit))
				}
				image = formatLockImages(e.Images)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, source, image)
	}
	return nil
}

// shortCommit abbreviates a git commit SHA for display.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// formatLockImages renders lockfile images as "ref@digest" (digest abbreviated),
// joined by commas, or "-" if none.
func formatLockImages(images []catalog.LockImage) string {
	if len(images) == 0 {
		return "-"
	}
	var parts []string
	for _, im := range images {
		part := im.Ref
		if im.Digest != "" {
			d := im.Digest
			if len(d) > 19 { // "sha256:" + 12 hex
				d = d[:19]
			}
			part = fmt.Sprintf("%s@%s", im.Ref, d)
		}
		if im.Verified {
			part += " " + color.GreenString("✓verified")
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
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
	resolved, err := catalog.ResolveSource(src)
	if err != nil {
		return err
	}
	if w := catalog.SchemaWarning(resolved.Manifest); w != "" {
		fmt.Println(color.YellowString("Warning: " + w))
		fmt.Println()
	}
	renderManifest(resolved, src)
	return nil
}

// printModuleSummary prints a concise pre-install summary (incl. resolved commit
// + image) for an external module -- a TOFU/provenance prompt.
func printModuleSummary(src catalog.Source, resolved *catalog.ResolvedModule, overrides map[string]string) {
	manifest := resolved.Manifest
	bold := color.New(color.Bold)
	fmt.Println()
	bold.Println("Module to install:")
	fmt.Printf("  Source:  %s\n", src.Raw)
	fmt.Printf("  Name:    %s\n", manifest.Name)
	if manifest.Version != "" {
		fmt.Printf("  Version: %s\n", manifest.Version)
	}
	if resolved.Commit != "" {
		fmt.Printf("  Commit:  %s\n", resolved.Commit)
	}
	for _, img := range resolved.Images {
		fmt.Printf("  Image:   %s\n", img)
	}
	if len(manifest.NodeTags) > 0 {
		fmt.Printf("  Routing tags: %s\n", strings.Join(manifest.NodeTags, ", "))
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
// with the resolved source + commit + image(s) added at the top.
func renderManifest(resolved *catalog.ResolvedModule, src catalog.Source) {
	manifest := resolved.Manifest
	bold := color.New(color.Bold)

	bold.Print("Source:      ")
	fmt.Println(src.Raw)
	if resolved.Commit != "" {
		bold.Print("Commit:      ")
		fmt.Println(resolved.Commit)
	}
	for _, img := range resolved.Images {
		bold.Print("Image:       ")
		fmt.Println(img)
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

	if len(manifest.NodeTags) > 0 {
		fmt.Println()
		bold.Print("Routing tags (node_tags): ")
		fmt.Println(strings.Join(manifest.NodeTags, ", "))
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

// scanResolvedRisks reads the resolved compose and returns its risk findings.
func scanResolvedRisks(resolved *catalog.ResolvedModule) []catalog.ComposeRisk {
	data, err := os.ReadFile(resolved.ComposePath)
	if err != nil {
		return nil
	}
	return catalog.ScanComposeRisks(string(data))
}

// criticalDirectiveNames returns the directive names of the Critical findings.
func criticalDirectiveNames(risks []catalog.ComposeRisk) []string {
	var out []string
	for _, r := range risks {
		if r.Severity == catalog.SeverityCritical {
			out = append(out, r.Directive)
		}
	}
	return out
}

// printTrustAndRisks prints the trust banner and the risk findings for an
// external module source. Untrusted sources get a prominent warning that this
// installs and runs an arbitrary container with Docker-level (host root) access.
func printTrustAndRisks(src catalog.Source, trusted bool, risks []catalog.ComposeRisk) {
	bold := color.New(color.Bold)
	fmt.Println()
	if trusted {
		fmt.Printf("  %s %s\n", color.GreenString("✓ trusted source"), color.New(color.Faint).Sprintf("(%s)", src.Raw))
	} else {
		bold.Println(color.YellowString("⚠ UNTRUSTED SOURCE"))
		fmt.Println(color.YellowString("  Installing this module runs an arbitrary container on this node with"))
		fmt.Println(color.YellowString("  Docker-level (host root-equivalent) access. Only proceed if you trust"))
		fmt.Printf("%s\n", color.YellowString("  the source and have reviewed its compose. "))
		fmt.Printf("  %s\n", color.New(color.Faint).Sprintf("Trust it for next time: citadel module trust %s", trustSuggestion(src)))
	}

	if len(risks) > 0 {
		fmt.Println()
		bold.Println("  Compose risk findings:")
		for _, r := range risks {
			switch r.Severity {
			case catalog.SeverityCritical:
				fmt.Printf("    %s  %s — %s\n", color.RedString("CRITICAL"), r.Directive, r.Detail)
			default:
				fmt.Printf("    %s      %s — %s\n", color.YellowString("HIGH"), r.Directive, r.Detail)
			}
		}
	}
}

// trustSuggestion returns a sensible trust pattern to suggest for a source.
func trustSuggestion(src catalog.Source) string {
	if src.Kind == catalog.KindGitHub {
		return src.Owner + "/" + src.Repo
	}
	return src.Raw
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
			manifest, composeSrc, _, err := resolveModuleForTUI(src)
			if err != nil {
				return out, err
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
			// Trust state + compose risk findings for the form to surface.
			// Catalog sources are first-party (Tier 0): always trusted and exempt
			// from the privilege gate, so we don't scan/surface risks for them
			// (matching the CLI catalog path) -- no scary checkbox for a built-in.
			out.Trusted = catalog.IsTrusted(src)
			if src.Kind != catalog.KindCatalog {
				if data, rerr := os.ReadFile(composeSrc); rerr == nil {
					for _, r := range catalog.ScanComposeRisks(string(data)) {
						crit := r.Severity == catalog.SeverityCritical
						out.Risks = append(out.Risks, controlcenter.ModuleRisk{
							Critical:  crit,
							Directive: r.Directive,
							Detail:    r.Detail,
						})
						if crit {
							out.HasCriticalRisk = true
						}
					}
				}
			}
			return out, nil
		},
		Install: func(source string, overrides map[string]string, allowPrivileged bool) (string, error) {
			src, err := catalog.ParseSource(source)
			if err != nil {
				return "", err
			}
			manifest, composeSrc, resolved, err := resolveModuleForTUI(src)
			if err != nil {
				return "", err
			}

			nodeManifest, configDir, err := findOrCreateManifest()
			if err != nil {
				return "", err
			}
			// Parity with the CLI: report an already-installed module cleanly
			// instead of letting it surface as a confusing port conflict.
			if hasService(nodeManifest, manifest.Name) {
				return "", fmt.Errorf("module '%s' is already in the node manifest", manifest.Name)
			}
			servicesDir := filepath.Join(configDir, "services")

			// Signature gate (shared core): refuse before install if the matched
			// trust entry is a verified publisher requiring a signature. No-op for
			// sources without a signature requirement (incl. catalog/Tier 0).
			var lockImages []catalog.LockImage
			if resolved != nil {
				lockImages = catalog.BuildLockImages(resolved.Images)
			}
			verifyResult, verr := catalog.VerifyModule(src, lockImages)
			if verr != nil {
				return "", verr
			}
			if verifyResult.Verified {
				lockImages = markLockImagesVerified(lockImages)
			}

			// interactive=false: the TUI supplies all required config as
			// overrides, so a missing required var is an error, never a stdin read.
			// allowPrivileged is the user's explicit opt-in from the form; the
			// shared core still refuses a Critical compose if it is false.
			// Catalog sources are first-party (Tier 0) and exempt from the gate,
			// matching the CLI catalog path (catalog.Install passes true).
			allow := allowPrivileged
			if src.Kind == catalog.KindCatalog {
				allow = true
			}
			result, err := catalog.InstallFromManifest(manifest, composeSrc, servicesDir, overrides, false, allow)
			if err != nil {
				return "", err
			}
			// Merge the module's declared routing tags so it becomes routable.
			if err := addServiceToManifestWithTags(configDir, result.Name, manifest.NodeTags); err != nil {
				return "", fmt.Errorf("failed to update manifest: %w", err)
			}
			// Record provenance for external sources (best-effort).
			if resolved != nil {
				recordModuleLock(src, resolved, lockImages)
			}
			return result.Name, nil
		},
	}
}

// resolveModuleForTUI resolves a source for the TUI callbacks: for a catalog name
// it loads from the catalog cache (resolved is nil, no provenance); for an
// external source it clones/updates the repo and returns the full ResolvedModule.
func resolveModuleForTUI(src catalog.Source) (manifest *catalog.ServiceManifest, composeSrc string, resolved *catalog.ResolvedModule, err error) {
	if src.Kind == catalog.KindCatalog {
		manifest, err = catalog.LoadServiceManifest(src.Name)
		if err != nil {
			return nil, "", nil, err
		}
		composeSrc, _ = catalog.GetComposeFile(src.Name)
		return manifest, composeSrc, nil, nil
	}
	resolved, err = catalog.ResolveSource(src)
	if err != nil {
		return nil, "", nil, err
	}
	return resolved.Manifest, resolved.ComposePath, resolved, nil
}
