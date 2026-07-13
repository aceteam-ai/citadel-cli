// cmd/module_ops.go
//
// liveModuleOps is the LIVE reconcile.ModuleOps adapter for the MODULE_SET
// handler (aceteam-ai/aceteam#5280). It maps the reconcile engine's narrow
// side-effect surface (install / uninstall / start / stop / list) onto the
// EXISTING catalog + compose + lockfile + manifest machinery this repo already
// uses for `citadel module install` / `citadel run` / `citadel stop`.
//
// It lives in the cmd package (not internal/worker) because it depends on the
// cmd-level manifest edges (findOrCreateManifest, addServiceToManifestWithTags,
// startService, stopServiceByCompose) that the worker package cannot import
// without a cycle. The worker handler stays decoupled and testable via the
// injected reconcile.ModuleOps interface; cmd wires this concrete adapter in
// registerPrivilegedNodeJobHandlers.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/reconcile"
	"github.com/aceteam-ai/citadel-cli/internal/redisapi"
)

// newReconcileLoop builds the config-gated desired-state PULL reconcile loop
// (aceteam#4273), or returns nil when the operator has not opted in
// (CITADEL_RECONCILE_PULL unset/false) so the feature adds zero cost by default.
//
// When enabled it wires the ProtoProvider (fetch DesiredState + report
// ActualState as protobuf over the device-authed client) onto the SAME live
// ModuleOps adapter and reconcile engine the MODULE_SET handler uses, so a pulled
// desired state converges through exactly the tested install/uninstall/start/stop
// machinery. RefuseFullWipe is set so an empty/misconfigured control plane cannot
// uninstall every module on the node.
//
// It must be wired in the WORKER path only (never also the control center): the
// converge loop is not idempotent telemetry, and two loops on one node would
// double-apply install/uninstall. The backend serve endpoint does not exist yet,
// so even when enabled the loop's fetches error until that paired follow-up ships.
func newReconcileLoop(client *redisapi.Client, nodeID string) *reconcile.Loop {
	if !reconcile.PullEnabled() {
		return nil
	}
	if client == nil || nodeID == "" {
		return nil
	}
	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	provider := reconcile.NewProtoProvider(client, client, nodeID, Version)
	rec := reconcile.NewReconciler(provider, newLiveModuleOps(log), nodeID)
	rec.RefuseFullWipe = true
	return reconcile.NewLoop(reconcile.Config{Enabled: true, Node: nodeID}, rec)
}

// liveModuleOps implements reconcile.ModuleOps against the real node.
type liveModuleOps struct {
	log func(format string, args ...any)

	// Injectable seams so the container-touching operations can be stubbed in
	// tests (the pure manifest/lockfile logic is exercised through them).
	startFn     func(name, composePath string) error
	composeDown func(composePath string, remove bool) error
	isRunning   func(name string) bool
}

// newLiveModuleOps builds the live adapter wired to this node's real edges.
func newLiveModuleOps(log func(format string, args ...any)) *liveModuleOps {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &liveModuleOps{
		log:         log,
		startFn:     startService,         // cmd/service.go: docker compose up -d
		composeDown: stopServiceByCompose, // cmd/stop.go: docker compose down
		isRunning:   containerIsRunning,   // docker inspect state
	}
}

// Install installs (or updates) a module from the assignment's Source with its
// Config overrides, then starts it. After a successful Install the module is
// RUNNING (the engine issues a follow-up Stop when the desired status is
// stopped). An already-installed module is updated in place via uninstall-then-
// install so its host ports free and its compose is replaced cleanly.
func (o *liveModuleOps) Install(ctx context.Context, m reconcile.ModuleAssignment) error {
	src, err := catalog.ParseSource(m.Source)
	if err != nil {
		return fmt.Errorf("parse source %q: %w", m.Source, err)
	}

	// Resolve + verify BEFORE any teardown. The network-dependent work (git clone
	// / cosign verify) is exactly what can fail transiently, so it must happen
	// while any existing module is still installed and running -- otherwise a
	// transient failure during a routine config update would delete a running
	// module and (on retry) leave it uninstalled. See the update-in-place teardown
	// below, which is keyed on the RESOLVED manifest.Name (stable across
	// source-ref/basename differences) and runs only after resolve+verify succeed.
	manifest, composeSrc, resolved, err := resolveModuleForTUI(src)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", m.Source, err)
	}

	nodeManifest, configDir, err := findOrCreateManifest()
	if err != nil {
		return fmt.Errorf("initialize node config: %w", err)
	}
	servicesDir := filepath.Join(configDir, "services")

	trusted := catalog.IsTrusted(src)
	untrusted := !trusted
	// Catalog (Tier-0) sources are first-party and exempt from the privilege gate
	// (they have no --allow-privileged flag), matching the CLI/TUI catalog path.
	// External sources keep the hard gate: a Critical compose is REFUSED here
	// (this is a remote, non-interactive apply -- there is no operator to
	// --allow-privileged, so a privileged external module fails with a clear
	// error rather than silently running with host-root access).
	allowPrivileged := src.Kind == catalog.KindCatalog

	// Signature gate (shared core): verify a verified-publisher signature by
	// digest before install; a no-op for sources with no signature requirement.
	var lockImages []catalog.LockImage
	if resolved != nil {
		lockImages = catalog.BuildLockImages(resolved.Images)
	}
	verifyResult, err := catalog.VerifyModule(src, lockImages)
	if err != nil {
		return fmt.Errorf("verify %q: %w", manifest.Name, err)
	}
	if verifyResult.Verified {
		lockImages = markLockImagesVerified(lockImages)
	}

	// Update-in-place: reconcile drives ActionUpdate (source/config drift) through
	// Install. If the RESOLVED module name is already installed, uninstall it now
	// -- AFTER the fallible resolve+verify -- so the fresh install does not trip
	// the port-conflict / already-in-manifest guards. Keying on the resolved
	// manifest.Name (not a source basename) makes this correct even when the
	// service name differs from the source basename or changes across refs.
	// Residual (interim, acceptable): if this Uninstall succeeds but the
	// InstallFromManifest below then fails, the module is left down until the
	// job retries and reinstalls it.
	if hasService(nodeManifest, manifest.Name) {
		o.log("MODULE_SET: %q already installed; updating in place", manifest.Name)
		if err := o.Uninstall(ctx, manifest.Name); err != nil {
			return fmt.Errorf("update %q: uninstall existing: %w", manifest.Name, err)
		}
	}

	// Non-interactive install: m.Config supplies the overrides; a missing REQUIRED
	// config var is a returned error (never a stdin prompt on a headless node).
	result, err := catalog.InstallFromManifest(manifest, composeSrc, servicesDir, m.Config, false, allowPrivileged, untrusted, false)
	if err != nil {
		return fmt.Errorf("install %q: %w", manifest.Name, err)
	}

	// Register in the manifest (merging the module's declared routing tags).
	if err := addServiceToManifestWithTags(configDir, result.Name, manifest.NodeTags); err != nil {
		return fmt.Errorf("register %q in manifest: %w", result.Name, err)
	}

	// Record provenance so a re-run does not see spurious drift. CRITICAL: store
	// the REQUESTED source form (src.Raw) and the config, so ListInstalled reports
	// the same canonical Source + Config the desired assignment carries and the
	// engine converges to a no-op on the next pass.
	o.recordLock(src, resolved, result, lockImages, m.Config)

	// A fresh install/update is RUNNING: clear any stale stopped marker, then
	// compose up. (The engine will follow with Stop if desired is stopped.)
	if err := setServiceDesiredStatus(configDir, result.Name, ""); err != nil {
		o.log("MODULE_SET: could not clear stopped marker for %q: %v", result.Name, err)
	}
	composePath := filepath.Join(servicesDir, result.Name+".yml")
	if err := o.startFn(result.Name, composePath); err != nil {
		return fmt.Errorf("start %q: %w", result.Name, err)
	}
	return nil
}

// Uninstall removes an installed module by name: compose down + drop it from the
// node manifest + delete its lockfile entry + remove its materialized files. It
// is the NET-NEW uninstall primitive (no imperative uninstall existed before).
// Idempotent: uninstalling a module that is not installed is a no-op success.
func (o *liveModuleOps) Uninstall(ctx context.Context, name string) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		// No manifest => nothing is installed => idempotent no-op.
		o.log("MODULE_SET: uninstall %q: no manifest, treating as no-op", name)
		return nil
	}

	var composeRel string
	found := false
	for _, s := range manifest.Services {
		if s.Name == name {
			composeRel = s.ComposeFile
			found = true
			break
		}
	}
	if !found {
		o.log("MODULE_SET: uninstall %q: not installed, no-op", name)
		return nil
	}

	// Compose down (stop + remove containers, keep named volumes). A missing
	// compose file means the stack is already gone -> proceed to de-register. A
	// real `docker compose down` failure (e.g. docker daemon down) is TRANSIENT:
	// return it so the job retries and we do not de-register a still-running
	// stack.
	if composeRel != "" {
		composePath := filepath.Join(configDir, composeRel)
		if _, statErr := os.Stat(composePath); statErr == nil {
			if err := o.composeDown(composePath, false); err != nil {
				return fmt.Errorf("compose down %q: %w", name, err)
			}
		}
	}

	// De-register from the manifest.
	if err := removeServiceFromManifest(configDir, name); err != nil {
		return fmt.Errorf("remove %q from manifest: %w", name, err)
	}
	// Delete the lockfile provenance entry (idempotent, best-effort).
	if err := catalog.DeleteLockEntry(name); err != nil {
		o.log("MODULE_SET: could not delete lock entry for %q: %v", name, err)
	}
	// Remove the materialized compose / sandbox / env files (best-effort).
	o.removeServiceFiles(configDir, name)
	return nil
}

// Start brings an already-installed module up and clears its durable stopped
// marker so it also starts on the next boot.
func (o *liveModuleOps) Start(ctx context.Context, name string) error {
	_, configDir, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if err := setServiceDesiredStatus(configDir, name, ""); err != nil {
		return err
	}
	composePath := o.composePathFor(configDir, name)
	if composePath == "" {
		return fmt.Errorf("start %q: no compose file in manifest", name)
	}
	return o.startFn(name, composePath)
}

// Stop brings an already-installed module down WITHOUT uninstalling it, and marks
// it durably stopped so the boot-time service-start paths skip it (the stop
// survives a reboot -- the sharp risk this handler must not silently regress).
func (o *liveModuleOps) Stop(ctx context.Context, name string) error {
	_, configDir, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	// Mark durable-stopped FIRST so that even if the compose-down below is
	// interrupted, a reboot will not resurrect the service.
	if err := setServiceDesiredStatus(configDir, name, "stopped"); err != nil {
		return err
	}
	composePath := o.composePathFor(configDir, name)
	if composePath == "" {
		// Installed but no compose file recorded: the marker alone makes it durable.
		return nil
	}
	if _, statErr := os.Stat(composePath); statErr != nil {
		return nil
	}
	return o.composeDown(composePath, false)
}

// ListInstalled reports the actual on-node state of every installed module,
// joining the manifest (source of truth for what is installed) with the lockfile
// (canonical Source / Ref / Config provenance). CANONICAL-FORM CONTRACT: Source
// is the REQUESTED source string the module was installed from (lockfile Source,
// i.e. src.Raw), so it diffs equal against a desired assignment expressed in the
// same requested form. Health reflects the durable stopped marker first, then the
// live container run-state.
func (o *liveModuleOps) ListInstalled(ctx context.Context) ([]reconcile.InstalledModule, error) {
	manifest, _, err := findAndReadManifest()
	if err != nil {
		// No manifest => nothing installed. Not an error for the reconciler.
		return nil, nil
	}
	lf, _ := catalog.LoadLockfile() // best-effort; nil-safe below

	out := make([]reconcile.InstalledModule, 0, len(manifest.Services))
	for _, s := range manifest.Services {
		im := reconcile.InstalledModule{Name: s.Name}
		if lf != nil {
			if e, ok := lf.LookupLock(s.Name); ok {
				im.Source = e.Source
				im.Ref = e.Ref
				im.Commit = e.Commit
				im.Config = e.Config
			}
		}
		// Catalog / embedded services carry no lockfile entry: their source IS the
		// service name (a bare catalog name), which is also what NameFromSource
		// derives, so a desired assignment with source == name diffs equal.
		if im.Source == "" {
			im.Source = s.Name
		}
		// Health: a durable stopped marker wins; otherwise reflect the container.
		if serviceStartDisabled(s) {
			im.Health = reconcile.HealthStopped
		} else if o.isRunning(s.Name) {
			im.Health = reconcile.HealthRunning
		} else {
			im.Health = reconcile.HealthStopped
		}
		out = append(out, im)
	}
	return out, nil
}

// recordLock upserts provenance for a freshly installed/updated module, carrying
// the REQUESTED source form + config so the reconciler sees no spurious drift.
// Best-effort: a lockfile write failure is logged, not fatal to the install.
func (o *liveModuleOps) recordLock(src catalog.Source, resolved *catalog.ResolvedModule, result *catalog.InstallResult, images []catalog.LockImage, config map[string]string) {
	entry := catalog.LockEntry{
		Name:      result.Name,
		Source:    src.Raw,
		Ref:       src.Ref,
		Config:    config,
		Sandboxed: result.Sandboxed,
	}
	if resolved != nil {
		entry.ResolvedRef = resolved.ResolvedRef
		entry.Commit = resolved.Commit
		if images == nil {
			images = catalog.BuildLockImages(resolved.Images)
		}
		entry.Images = images
	}
	if err := catalog.UpsertLockEntry(entry); err != nil {
		o.log("MODULE_SET: could not record provenance for %q: %v", result.Name, err)
	}
}

// removeServiceFiles removes a module's materialized compose, sandbox override,
// and env files from the services directory. Best-effort: missing files are fine.
func (o *liveModuleOps) removeServiceFiles(configDir, name string) {
	servicesDir := filepath.Join(configDir, "services")
	for _, suffix := range []string{".yml", ".sandbox.yml", ".env"} {
		path := filepath.Join(servicesDir, name+suffix)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			o.log("MODULE_SET: could not remove %s: %v", path, err)
		}
	}
}

// composePathFor returns the absolute compose path for a manifest service, or ""
// if the service is not in the manifest or has no compose file.
func (o *liveModuleOps) composePathFor(configDir, name string) string {
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return ""
	}
	for _, s := range manifest.Services {
		if s.Name == name && s.ComposeFile != "" {
			return filepath.Join(configDir, s.ComposeFile)
		}
	}
	return ""
}

// containerIsRunning reports whether the module's citadel-<name> container is up.
// Best-effort: returns false if the engine is unavailable or the query fails.
func containerIsRunning(name string) bool {
	rt := catalog.SelectContainerRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, rt.EngineBin, "inspect", "--format", "{{.State.Running}}", "citadel-"+name).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// Ensure liveModuleOps implements reconcile.ModuleOps.
var _ reconcile.ModuleOps = (*liveModuleOps)(nil)
