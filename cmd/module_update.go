// cmd/module_update.go
//
// Module lifecycle commands (issue #346), registered under the `citadel module`
// group in this file's own init() to minimize conflicts with cmd/module.go:
//
//	citadel module update [<name>]   re-resolve installed module(s); re-install
//	                                  when the resolved commit changed. Health
//	                                  rollback on a definitively-unhealthy update.
//	citadel module update --prune     additionally GC unreferenced cache dirs.
//	citadel module gc                 GC unreferenced module cache dirs.
//	citadel module list --outdated    mark installed modules whose locked commit
//	                                  is behind the source's current resolution.
//
// All resolution/diff/gc/rollback decision logic lives in internal/catalog
// (semver.go, lifecycle.go, health.go) as pure, table-tested helpers; this file
// is thin wiring.
package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var (
	moduleUpdatePrune   bool
	moduleUpdateRestart bool
	moduleListOutdated  bool
)

var moduleUpdateCmd = &cobra.Command{
	Use:   "update [<name>]",
	Short: "Re-resolve installed modules and re-install any that changed",
	Long: `Re-resolve each installed module's source from the lockfile and re-install
any whose resolved commit changed (constraint/channel refs re-resolve against the
latest tags). Prints what changed.

If a re-installed module declares a health_check and is currently running, it is
restarted and probed; on a definitively-unhealthy result the previous version is
restored (best-effort rollback).`,
	Args: cobra.MaximumNArgs(1),
	RunE: runModuleUpdate,
}

var moduleGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Remove module cache directories not referenced by the lockfile",
	Args:  cobra.NoArgs,
	RunE:  runModuleGC,
}

func init() {
	// Register under the existing `module` group defined in cmd/module.go.
	moduleCmd.AddCommand(moduleUpdateCmd)
	moduleCmd.AddCommand(moduleGCCmd)

	moduleUpdateCmd.Flags().BoolVar(&moduleUpdatePrune, "prune", false,
		"After updating, garbage-collect cache dirs no longer referenced by the lockfile.")
	moduleUpdateCmd.Flags().BoolVar(&moduleUpdateRestart, "restart", true,
		"Restart a changed module's container if it is currently running (needed for the health-rollback probe).")

	// Extend the existing `module list` with --outdated (the command itself is
	// defined in cmd/module.go; only the flag is added here).
	moduleListCmd.Flags().BoolVar(&moduleListOutdated, "outdated", false,
		"Check each installed module against its source and mark outdated ones.")
}

// runModuleUpdate re-resolves installed modules and re-installs the changed ones.
func runModuleUpdate(cmd *cobra.Command, args []string) error {
	lf, err := catalog.LoadLockfile()
	if err != nil {
		return err
	}
	if len(lf.Modules) == 0 {
		fmt.Println("No modules to update (modules.lock is empty).")
		return nil
	}

	_, configDir, err := findOrCreateManifest()
	if err != nil {
		return fmt.Errorf("failed to load node configuration: %w", err)
	}
	servicesDir := filepath.Join(configDir, "services")

	var only string
	if len(args) == 1 {
		only = args[0]
	}

	updated, checked := 0, 0
	for _, entry := range lf.Modules {
		if only != "" && entry.Name != only {
			continue
		}
		// Only attempt modules that are external sources (have a parseable,
		// non-catalog source). Catalog/embedded services are not lifecycle-managed
		// here.
		src, perr := catalog.SourceFromLock(entry)
		if perr != nil {
			continue
		}
		if src.Kind == catalog.KindCatalog {
			continue
		}
		checked++
		if updateOne(entry, src, servicesDir) {
			updated++
		}
	}

	if only != "" && checked == 0 {
		return fmt.Errorf("module %q is not a lifecycle-managed (external-source) installed module", only)
	}
	fmt.Printf("\nChecked %d module(s); %d updated.\n", checked, updated)

	if moduleUpdatePrune {
		fmt.Println()
		if err := pruneUnreferenced(); err != nil {
			fmt.Fprintf(os.Stderr, "prune: %v\n", err)
		}
	}
	return nil
}

// updateOne re-resolves one module and re-installs it if its commit changed.
// Returns true if it re-installed. Best-effort: per-module failures are logged
// and do not abort the whole update.
func updateOne(entry catalog.LockEntry, src catalog.Source, servicesDir string) bool {
	fmt.Printf("Checking %s (%s)...\n", color.CyanString(entry.Name), entry.Source)
	resolved, err := catalog.ResolveSource(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  could not resolve %s: %v\n", entry.Name, err)
		return false
	}

	switch catalog.CompareCommits(entry.Commit, resolved.Commit) {
	case catalog.UpdateUnchanged:
		fmt.Printf("  up-to-date (%s)\n", shortCommit(entry.Commit))
		return false
	case catalog.UpdateUnknown:
		fmt.Printf("  %s\n", color.YellowString("could not determine change (missing commit info); skipping"))
		return false
	}

	resolvedLabel := resolved.Commit
	if resolved.ResolvedRef != "" {
		resolvedLabel = fmt.Sprintf("%s (%s)", resolved.ResolvedRef, shortCommit(resolved.Commit))
	}
	fmt.Printf("  changed: %s -> %s\n", shortCommit(entry.Commit), resolvedLabel)

	wasRunning := moduleContainerRunning("citadel-" + entry.Name)

	// Re-install (copies the new compose/env). interactive=false (scripted).
	// allowPrivileged mirrors the recorded behavior: if the original install was
	// allowed (it's installed), keep it installable -- but the shared-core gate
	// still refuses a newly-introduced Critical compose unless allowed. We pass
	// false so a module that *becomes* privileged on update is refused, which is
	// the safe default; the operator can re-run `module install` with the flag.
	if _, err := catalog.InstallFromManifest(resolved.Manifest, resolved.ComposePath, servicesDir, nil, false, false); err != nil {
		fmt.Fprintf(os.Stderr, "  re-install failed for %s: %v\n", entry.Name, err)
		return false
	}

	newEntry := catalog.LockEntry{
		Name:        resolved.Manifest.Name,
		Source:      entry.Source,
		Ref:         entry.Ref,
		ResolvedRef: resolved.ResolvedRef,
		Commit:      resolved.Commit,
		Images:      catalog.BuildLockImages(resolved.Images),
	}
	if err := catalog.UpsertLockEntry(newEntry); err != nil {
		fmt.Fprintf(os.Stderr, "  could not update lockfile for %s: %v\n", entry.Name, err)
	}

	// Health rollback: only if the module declares a health_check, was already
	// running, and we are allowed to restart it (so the probe is meaningful).
	if moduleUpdateRestart && wasRunning && catalog.HasHealthProbe(resolved.Manifest.HealthCheck) {
		if err := composeUpDetached(entry.Name, filepath.Join(servicesDir, entry.Name+".yml")); err != nil {
			fmt.Fprintf(os.Stderr, "  restart failed for %s: %v\n", entry.Name, err)
		} else if catalog.ProbeHealth(resolved.Manifest.HealthCheck) == catalog.ProbeUnhealthy {
			fmt.Printf("  %s\n", color.RedString("health check failed after update; rolling back"))
			rollback(entry, servicesDir)
			return false
		}
	}

	fmt.Printf("  %s\n", color.GreenString("updated"))
	return true
}

// rollback restores a module to its previously-locked commit and re-installs +
// restarts it. Best-effort and clearly logged: a failure leaves the new (failing)
// version in place but reports the situation so the operator can intervene.
func rollback(prev catalog.LockEntry, servicesDir string) {
	src, err := catalog.SourceAtCommit(prev, prev.Commit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  rollback aborted: %v\n", err)
		return
	}
	resolved, err := catalog.ResolveSource(src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  rollback failed to re-resolve previous version: %v\n", err)
		return
	}
	if _, err := catalog.InstallFromManifest(resolved.Manifest, resolved.ComposePath, servicesDir, nil, false, true); err != nil {
		fmt.Fprintf(os.Stderr, "  rollback re-install failed: %v\n", err)
		return
	}
	// Restore the lockfile to the previous provenance.
	if err := catalog.UpsertLockEntry(prev); err != nil {
		fmt.Fprintf(os.Stderr, "  rollback could not restore lockfile: %v\n", err)
	}
	if err := composeUpDetached(prev.Name, filepath.Join(servicesDir, prev.Name+".yml")); err != nil {
		fmt.Fprintf(os.Stderr, "  rollback restart failed: %v\n", err)
	}
	fmt.Printf("  %s %s\n", color.YellowString("rolled back to"), shortCommit(prev.Commit))
}

// runModuleGC removes module cache directories not referenced by the lockfile.
func runModuleGC(cmd *cobra.Command, args []string) error {
	return pruneUnreferenced()
}

// pruneUnreferenced is the shared GC body for `module gc` and `update --prune`.
func pruneUnreferenced() error {
	lf, err := catalog.LoadLockfile()
	if err != nil {
		return err
	}
	present, err := catalog.ListCacheDirs()
	if err != nil {
		return err
	}
	if len(present) == 0 {
		fmt.Println("Module cache is empty; nothing to collect.")
		return nil
	}
	referenced := catalog.ReferencedCacheDirs(lf.Modules)
	candidates := catalog.GCCandidates(present, referenced)
	if len(candidates) == 0 {
		fmt.Printf("Module cache: %d dir(s), all referenced; nothing to collect.\n", len(present))
		return nil
	}
	removed, err := catalog.PruneCache(candidates)
	for _, name := range removed {
		fmt.Printf("  removed %s\n", name)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Garbage-collected %d unreferenced cache dir(s).\n", len(removed))
	return nil
}

// composeUpDetached starts/recreates a service's container non-interactively. It
// is intentionally minimal (no prompts) so `module update` is scriptable.
func composeUpDetached(name, composePath string) error {
	if _, err := os.Stat(composePath); err != nil {
		return fmt.Errorf("compose file not found: %s", composePath)
	}
	c := exec.Command("docker", "compose", "-f", composePath, "-p", "citadel-"+name, "up", "-d")
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("docker compose up failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// containerRunning reports whether a container with the given name is currently
// running. Best-effort: false if docker is unavailable.
func moduleContainerRunning(containerName string) bool {
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "running"
}

// printOutdated implements `module list --outdated`: it re-resolves each
// lifecycle-managed module and prints a status table. It is invoked from
// runModuleList (cmd/module.go) when --outdated is set.
func printOutdated() error {
	lf, err := catalog.LoadLockfile()
	if err != nil {
		return err
	}
	if lf == nil || len(lf.Modules) == 0 {
		fmt.Println("No lifecycle-managed modules in modules.lock.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	color.New(color.Bold).Fprintf(w, "MODULE\tSOURCE\tLOCKED\tRESOLVED\tSTATUS\n")

	any := false
	for _, entry := range lf.Modules {
		src, perr := catalog.SourceFromLock(entry)
		if perr != nil || src.Kind == catalog.KindCatalog {
			continue
		}
		any = true
		resolved, err := catalog.ResolveSource(src)
		if err != nil {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", entry.Name, entry.Source,
				shortCommit(entry.Commit), "-", color.YellowString("error"))
			continue
		}
		resolvedLabel := shortCommit(resolved.Commit)
		if resolved.ResolvedRef != "" {
			resolvedLabel = fmt.Sprintf("%s/%s", resolved.ResolvedRef, shortCommit(resolved.Commit))
		}
		status := ""
		switch catalog.CompareCommits(entry.Commit, resolved.Commit) {
		case catalog.UpdateUnchanged:
			status = color.GreenString("up-to-date")
		case catalog.UpdateChanged:
			status = color.YellowString("outdated")
		default:
			status = "unknown"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", entry.Name, entry.Source,
			shortCommit(entry.Commit), resolvedLabel, status)
	}
	if !any {
		w.Flush()
		fmt.Println("No lifecycle-managed (external-source) modules installed.")
	}
	return nil
}
