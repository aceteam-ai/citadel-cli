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

// newReconcileLoop builds the desired-state PULL reconcile loop (aceteam#4273).
// The loop is ON by default; it returns nil only when the operator has hit the
// CITADEL_RECONCILE_PULL kill switch, or when there is no client / node ID to
// key it by. A node with no desired-state rows converges to a no-op (see
// RefuseFullWipe below), so default-on adds no churn for unmanaged nodes.
//
// It wires the ProtoProvider (fetch DesiredState + report
// ActualState as protobuf over the device-authed client) onto the SAME live
// ModuleOps adapter and reconcile engine the MODULE_SET handler uses, so a pulled
// desired state converges through exactly the tested install/uninstall/start/stop
// machinery. RefuseFullWipe is set so an empty/misconfigured control plane cannot
// uninstall every module on the node.
//
// It must be wired in the WORKER path only (never also the control center): the
// converge loop is not idempotent telemetry, and two loops on one node would
// double-apply install/uninstall.
//
// nodeID MUST be the Headscale numeric node ID (e.g. "1084"), NOT the hostname
// (aceteam#535). The desired-state serve endpoint matches rows by a raw
// `.eq("node_id", <path param>)` against `fabric_node_status.node_id` (the
// Headscale numeric ID the backend upserts from heartbeats); a hostname never
// matches, leaving the loop non-functional. Returns nil when nodeID is empty so
// the loop is skipped rather than started under a wrong key.
func newReconcileLoop(client *redisapi.Client, nodeID string) *reconcile.Loop {
	if reconcile.PullDisabled() {
		return nil
	}
	if client == nil || nodeID == "" {
		return nil
	}
	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	provider := reconcile.NewProtoProvider(client, client, nodeID, Version)
	rec := reconcile.NewReconciler(provider, newLiveModuleOps(log), nodeID)
	rec.RefuseFullWipe = true
	// Replace the default silent tracker with one that actually prints
	// (citadel-cli#742): loud once on refused<->ok transitions, a periodic
	// summary while a refusal persists -- see reconcile.HealthTracker's doc
	// for why this replaces the old "identical WARN every pass" behavior.
	rec.Health = reconcile.NewHealthTracker(func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "   - "+format+"\n", args...)
	})
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
	// Node-generated secrets must survive the teardown below: Uninstall deletes
	// <name>.env, so without carrying them forward here the re-install would mint
	// a NEW value on every re-assignment -- and compose would then recreate only
	// the container whose env changed, leaving its consumer running with the old
	// credential in memory. Read BEFORE the uninstall; anything the assignment
	// supplies still wins, so an explicit rotation is still possible.
	installConfig := m.Config
	if hasService(nodeManifest, manifest.Name) {
		installConfig = catalog.CarryGeneratedConfig(manifest, servicesDir, m.Config)
		o.log("MODULE_SET: %q already installed; updating in place", manifest.Name)
		if err := o.Uninstall(ctx, manifest.Name); err != nil {
			return fmt.Errorf("update %q: uninstall existing: %w", manifest.Name, err)
		}
	}

	// Non-interactive install: installConfig supplies the overrides; a missing
	// REQUIRED config var is a returned error (never a stdin prompt on a headless
	// node).
	result, err := catalog.InstallFromManifest(manifest, composeSrc, servicesDir, installConfig, false, allowPrivileged, untrusted, false)
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

// ListInstalled reports the actual on-node state of every MODULE-MANAGED
// module -- i.e. every entry the module system itself installed, as recorded
// in the lockfile -- joined with the manifest for run-state (the durable
// stopped marker) and the live container inspection for health.
//
// CRITICAL SCOPING (citadel#739): the lockfile, not the manifest `services:`
// list, is the converge engine's authority for "what is installed". A node's
// manifest also lists services that were never module-managed -- started by
// `citadel run`, provisioned by `citadel init`, or an embedded engine brought
// up some other way -- and those carry no lockfile entry. Reconcile.Reconcile
// treats anything in `actual` but not in `desired` as drift to UNINSTALL, so
// enumerating the wider manifest set here (as this used to) meant the FIRST
// non-empty desired state control-plane-side would tear down every other
// manifest service the moment it didn't also appear in that desired set --
// the empty-desired-set full-wipe guard in internal/reconcile/loop.go does not
// cover this, because the desired set here is non-empty. Scoping to the
// lockfile keeps operator-run / embedded services permanently OUTSIDE
// reconcile's uninstall authority, exactly as internal/reconcile/ops.go's
// ModuleOps.ListInstalled doc already specified ("read modules.lock ... joined
// with the live container run-state") and exactly as the OTHER ActualState
// reporter, nodestate.BuildActualState, already does (see CLAUDE.md "Two
// ActualState reporters, two different module sets").
//
// CANONICAL-FORM CONTRACT: Source is the REQUESTED source string the module
// was installed from (lockfile Source, i.e. src.Raw), so it diffs equal
// against a desired assignment expressed in the same requested form. Health
// reflects the durable stopped marker first, then the live container
// run-state.
//
// Preserves the pre-existing "no manifest => nothing installed" short-circuit
// (unrelated to #739's fix; kept as-is to minimize the blast radius of a
// change to this destructive-adjacent path) -- a node with no manifest has no
// compose files to run anything from regardless of what the lockfile claims.
func (o *liveModuleOps) ListInstalled(ctx context.Context) ([]reconcile.InstalledModule, error) {
	manifest, _, err := findAndReadManifest()
	if err != nil {
		// No manifest => nothing installed. Not an error for the reconciler.
		return nil, nil
	}
	lf, err := catalog.LoadLockfile()
	if err != nil {
		// Can't read the module-system's install ledger: report nothing rather
		// than falling back to the wider (and, per #739, unsafe) manifest scan.
		// Mirrors nodestate.BuildActualState's "no readable lockfile -> report
		// zero modules" -- unconditional, crash-free, and it never widens the
		// converge engine's uninstall authority.
		o.log("MODULE_SET: could not read lockfile, reporting no installed modules: %v", err)
		return nil, nil
	}
	if lf == nil || len(lf.Modules) == 0 {
		return nil, nil
	}

	// The manifest is consulted ONLY to enrich run-state (the durable stopped
	// marker); it is never the enumeration source.
	servicesByName := make(map[string]Service, len(manifest.Services))
	for _, s := range manifest.Services {
		servicesByName[s.Name] = s
	}

	out := make([]reconcile.InstalledModule, 0, len(lf.Modules))
	for _, e := range lf.Modules {
		im := reconcile.InstalledModule{
			Name:   e.Name,
			Source: e.Source,
			Ref:    e.Ref,
			Commit: e.Commit,
			Config: e.Config,
		}
		// Pre-canonical-form lockfile entries (or a blank Source) fall back to
		// the module name, matching NameFromSource so a desired assignment with
		// source == name still diffs equal.
		if im.Source == "" {
			im.Source = e.Name
		}
		// Health: a durable stopped marker wins (when the manifest still lists
		// this module); otherwise reflect the live container.
		if s, ok := servicesByName[e.Name]; ok && serviceStartDisabled(s) {
			im.Health = reconcile.HealthStopped
		} else if o.isRunning(e.Name) {
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

// resolveModuleContainerName resolves the container name containerIsRunning
// (and any future module-health check) should inspect for a module named
// name. Split out from containerIsRunning so the name resolution -- the part
// citadel#860 changes -- is unit-testable without a container runtime.
//
// citadel#860: for an EMBEDDED service (services.ServiceMap), the container
// name is resolved via embeddedContainerName -- override-scoped
// ("citadel-<hash>-<name>") under an active --node-dir/CITADEL_NODE_DIR,
// unchanged "citadel-<name>" otherwise -- so this agrees with what
// ensureComposeFile just materialized. Catalog/third-party module names (not
// in ServiceMap) keep the plain "citadel-<name>" convention unconditionally:
// those compose files author their own container_name and are not namespaced
// by --node-dir (see cmd/nodedir.go's package doc).
func resolveModuleContainerName(name string) string {
	if isEmbeddedService(name) {
		return embeddedContainerName(name)
	}
	return "citadel-" + name
}

// containerIsRunning reports whether the module's container is up.
// Best-effort: returns false if the engine is unavailable or the query fails.
func containerIsRunning(name string) bool {
	rt := catalog.SelectContainerRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, rt.EngineBin, "inspect", "--format", "{{.State.Running}}", resolveModuleContainerName(name)).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// Ensure liveModuleOps implements reconcile.ModuleOps.
var _ reconcile.ModuleOps = (*liveModuleOps)(nil)
