// cmd/module_control.go
//
// `citadel module stop|start|restart <name>` gives an operator a targeted,
// single-module lever (aceteam#8248 part 1). Before this, freeing a node
// meant hand-editing citadel.yaml to add desired_status: stopped and then
// `systemctl --user restart citadel.service` -- a FULL worker restart that
// drops the node off the fabric and reconciles every module, just to stop
// one.
//
// This file does NOT introduce a new stop/start mechanism. It drives the
// SAME scoped, durable primitive the MODULE_SET reconcile handler already
// uses -- liveModuleOps.Start/Stop (cmd/module_ops.go) -- which:
//  1. Sets/clears the durable desired_status marker on ONLY the named
//     service (setServiceDesiredStatus), so the stop/start survives a
//     citadel process restart or a reboot: the boot-time service-start paths
//     (runAllServices in cmd/run.go, startManagedServices in cmd/work.go)
//     both skip a service marked stopped.
//  2. Drives `docker compose -f <that service's compose file> up|down`
//     (startService / stopServiceByCompose, cmd/service.go + cmd/stop.go)
//     -- scoped to the one compose file, which only ever declares that
//     service's own container(s) (#528: compose files share a project name
//     but `-f` scopes the up/down action to what that file declares).
//
// No other module is touched, the internal/reconcile engine's diff/apply is
// never invoked, and the citadel process/worker is never restarted -- this is
// a targeted action, not a converge pass.
//
// Scope caveat (not a bug, just a boundary this command does not cross): a
// node that ALSO runs the desired-state PULL reconcile loop
// (internal/reconcile, wired for lockfile-recorded modules only) still
// converges to whatever the CONTROL PLANE has assigned. If that module's
// assignment there is still "running", the next pull will see the marker set
// by this command (reconcile.HealthStopped) disagree with the desired
// "running" state and queue a start -- this command changes only the node's
// local desired state, not the control plane's. Durable against
// restart/reboot; not durable against a disagreeing control plane.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/services"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var moduleStopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a single module (scoped: no other module is touched, the worker is not restarted)",
	Long: `Stops one module by name.

Sets the module's durable desired_status:stopped marker in the manifest,
then brings down only that module's compose stack. The stop survives a
citadel process restart or a reboot -- 'citadel work'/'citadel run' both
honor the marker and skip the module on boot. Sibling modules keep
running and the node stays on the fabric -- this does NOT restart
'citadel work'/'citadel up' and does NOT touch any other module.

Note: if this node also runs the desired-state PULL reconcile loop and the
control plane's assignment for this module is still "running", that loop
will start it again on its next pass -- this command only sets the
node-local desired state, it does not change what the control plane has
assigned for the module.

Already-stopped is a clean no-op success.

Add --dry-run to preview what would happen (resolved node dir, compose file,
container(s)) without doing it. Add --expect-node <name-or-id> to refuse the
action (fail closed) unless the resolved node's identity matches -- a safety
check for scripted/agent use against a specific node.`,
	Example: `  citadel module stop vllm
  citadel module stop vllm --dry-run
  citadel module stop vllm --expect-node my-test-node`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleControl(cmd.Context(), args[0], moduleActionStop)
	},
	SilenceUsage: true,
}

var moduleStartCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a single module (scoped: no other module is touched, the worker is not restarted)",
	Long: `Starts one module by name.

Clears the module's durable desired_status:stopped marker in the manifest
(so it also starts again on the next boot), then brings up only that
module's compose stack. Sibling modules are untouched -- this does NOT
restart 'citadel work'/'citadel up' and does NOT touch any other module.

Already-running is a clean no-op success.

Add --dry-run to preview what would happen (resolved node dir, compose file,
container(s)) without doing it. Add --expect-node <name-or-id> to refuse the
action (fail closed) unless the resolved node's identity matches -- a safety
check for scripted/agent use against a specific node.

This IS the blessed recovery path for a stopped/crashed service, embedded
engine (vllm, bonsai, unlimited-ocr, ...) or catalog module alike -- never
hand-run 'docker compose up' directly: several embedded compose files require
citadel-injected env vars (e.g. host ports) that only citadel supplies.`,
	Example: `  citadel module start vllm
  citadel module start vllm --dry-run
  citadel module start vllm --expect-node my-test-node`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleControl(cmd.Context(), args[0], moduleActionStart)
	},
	SilenceUsage: true,
}

var moduleRestartCmd = &cobra.Command{
	Use:   "restart <name>",
	Short: "Restart a single module: stop then start it (scoped)",
	Long: `Restarts one module by name -- equivalent to 'citadel module stop <name>'
immediately followed by 'citadel module start <name>'. No other module is
touched and the worker is not restarted.

Add --dry-run to preview what would happen (resolved node dir, compose file,
container(s)) without doing it. Add --expect-node <name-or-id> to refuse the
action (fail closed) unless the resolved node's identity matches -- a safety
check for scripted/agent use against a specific node.

This IS the blessed recovery path for a stopped/crashed service, embedded
engine (vllm, bonsai, unlimited-ocr, ...) or catalog module alike -- never
hand-run 'docker compose up' directly.`,
	Example: `  citadel module restart vllm
  citadel module restart vllm --dry-run
  citadel module restart vllm --expect-node my-test-node`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleControl(cmd.Context(), args[0], moduleActionRestart)
	},
	SilenceUsage: true,
}

// moduleDryRun and moduleExpectNode back the --dry-run / --expect-node flags
// shared by stop/start/restart (citadel#853). Package-level like the other
// module_control.go action flags; only one of these three commands runs per
// invocation, so there is no cross-command interference.
var (
	moduleDryRun     bool
	moduleExpectNode string
)

func init() {
	moduleCmd.AddCommand(moduleStopCmd)
	moduleCmd.AddCommand(moduleStartCmd)
	moduleCmd.AddCommand(moduleRestartCmd)

	for _, c := range []*cobra.Command{moduleStopCmd, moduleStartCmd, moduleRestartCmd} {
		c.Flags().BoolVar(&moduleDryRun, "dry-run", false, "Print what would happen (resolved node dir, compose file, container(s)) without doing it")
		c.Flags().StringVar(&moduleExpectNode, "expect-node", "", "Refuse to act unless the resolved node's identity (name, hostname, or mesh ID) matches this value")
	}
}

// moduleAction is the requested single-module lifecycle action.
type moduleAction int

const (
	moduleActionStop moduleAction = iota
	moduleActionStart
	moduleActionRestart
)

func (a moduleAction) String() string {
	switch a {
	case moduleActionStop:
		return "stop"
	case moduleActionStart:
		return "start"
	case moduleActionRestart:
		return "restart"
	default:
		return "?"
	}
}

// runModuleControl is the shared entry point for `citadel module
// stop|start|restart <name>`. It validates the name against the node
// manifest (the source of truth for what is installed, catalog or
// git-sourced module alike -- matching `citadel stop`/`citadel run`'s
// existing single-service scope), applies the --expect-node safety gate and
// --dry-run preview (citadel#853), then dispatches to the scoped stop/start
// primitive. Kept separate from the cobra RunE closures so it is
// unit-testable without going through Cobra.
func runModuleControl(ctx context.Context, name string, action moduleAction) error {
	manifest, configDir, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("%w\nHint: run 'citadel init' first, or 'citadel module install <source>' to install a module", err)
	}

	if !hasService(manifest, name) {
		if _, embedded := services.ServiceMap[name]; embedded {
			// A never-started embedded engine is a real service this node CAN
			// run, but it is not yet in the manifest, so module control (which
			// only ever touches services already tracked there) can't target
			// it. Say so precisely rather than folding it into "unknown
			// module" -- 'citadel run' is the command that adds + starts it.
			return fmt.Errorf("module %q is a known embedded service but is not yet in this node's manifest "+
				"(never started here). Run 'citadel run %s' to add and start it; 'citadel module start %s' "+
				"will work once it is in the manifest", name, name, name)
		}
		names := manifestServiceNames(manifest)
		if len(names) == 0 {
			return fmt.Errorf("unknown module %q: no modules are installed on this node", name)
		}
		return fmt.Errorf("unknown module %q. Installed modules: %s", name, strings.Join(names, ", "))
	}

	svc := lookupService(manifest, name)
	if svc.ComposeFile == "" {
		return fmt.Errorf("module %q is not a docker-compose-managed service (no compose_file recorded in the manifest); "+
			"'citadel module %s' only manages compose-based modules -- use 'citadel stop'/'citadel run' for native services", name, action)
	}

	// --expect-node (citadel#853): fail CLOSED before anything else runs (even
	// before printing a --dry-run plan -- a preview that a real run would
	// refuse is the wrong direction of error) if the resolved node's identity
	// does not match. Reuses #844's whoami identity resolution rather than
	// reinventing node-identity logic -- but only when the fast local check
	// (manifest node name, itself --node-dir-aware, + OS hostname) doesn't
	// already settle it: gatherIdentity performs a live ~5s network probe
	// (VerifyOrReconnect), which is unnecessary cost -- and an unnecessary
	// tsnet side effect -- for the common case of matching by name.
	if moduleExpectNode != "" {
		matched := expectNodeMatchesFast(manifest, moduleExpectNode)
		var id NodeIdentity
		if !matched {
			id = gatherIdentity(ctx)
			matched = nodeIdentityMatches(id, moduleExpectNode)
		}
		if !matched {
			return fmt.Errorf("refusing to %s module %q: --expect-node %q does not match this node "+
				"(name=%q hostname=%q mesh-id=%q, resolved node dir=%s)",
				action, name, moduleExpectNode, id.NodeName, id.Hostname, id.HeadscaleNodeID, configDir)
		}
	}

	composePath := filepath.Join(configDir, svc.ComposeFile)
	if moduleDryRun {
		fmt.Print(moduleDryRunPlan(name, action, configDir, composePath))
		return nil
	}

	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	ops := newLiveModuleOps(log)
	return dispatchModuleAction(ops, name, action)
}

// expectNodeMatchesFast checks --expect-node against purely local, instant
// signals -- the manifest node name (which is itself --node-dir-aware, since
// it came from the same findAndReadManifest call the caller already made) and
// the OS hostname -- without gatherIdentity's live network probe. This covers
// the common case (an agent/test target keyed by node name, exactly the
// --node-dir + --expect-node pairing this feature is for) at zero cost; only
// a numeric-mesh-ID expectation needs the slower fallback.
func expectNodeMatchesFast(manifest *CitadelManifest, expect string) bool {
	if manifest != nil && strings.EqualFold(strings.TrimSpace(manifest.Node.Name), expect) {
		return true
	}
	if hostname, err := os.Hostname(); err == nil && strings.EqualFold(hostname, expect) {
		return true
	}
	return false
}

// nodeIdentityMatches reports whether expect (an --expect-node value) matches
// any of the resolved node's known identifiers: manifest node name (honors
// --node-dir, since it flows through findAndReadManifest), OS hostname, or the
// live Headscale mesh node ID. Case-insensitive. An empty expect always
// matches (the "no gate configured" case, though callers only invoke this when
// moduleExpectNode is non-empty).
func nodeIdentityMatches(id NodeIdentity, expect string) bool {
	expect = strings.TrimSpace(expect)
	if expect == "" {
		return true
	}
	for _, candidate := range []string{id.NodeName, id.Hostname, id.HeadscaleNodeID} {
		if candidate != "" && strings.EqualFold(candidate, expect) {
			return true
		}
	}
	return false
}

// moduleDryRunPlan renders the --dry-run preview text for a module
// stop/start/restart: the resolved node dir, the compose file, and the
// container(s) it declares -- everything an operator needs to confirm the
// blast radius before re-running without --dry-run. Returns a string (rather
// than printing directly) so it is directly testable.
func moduleDryRunPlan(name string, action moduleAction, configDir, composePath string) string {
	var verb string
	switch action {
	case moduleActionStop:
		verb = "stop"
	case moduleActionStart:
		verb = "start"
	case moduleActionRestart:
		verb = "stop then start (restart)"
	default:
		verb = action.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "--- DRY RUN: would %s module '%s' ---\n", verb, name)
	fmt.Fprintf(&b, "  Resolved node dir: %s\n", configDir)
	fmt.Fprintf(&b, "  Compose file:      %s\n", composePath)
	fmt.Fprintf(&b, "  Container(s):      %s\n", strings.Join(dryRunContainerNames(composePath, name), ", "))
	fmt.Fprintln(&b, "No changes made.")
	return b.String()
}

// dryRunContainerNames returns the container name(s) a module's compose file
// declares via explicit container_name: fields -- the authoritative value
// docker actually acts on (see CLAUDE.md's #692 note on why the
// "citadel-<name>" convention alone cannot be trusted). Falls back to that
// convention only when the compose file is unreadable/unparseable or declares
// no explicit container_name; this is informational-only (the dry-run
// preview), never used to decide what the real stop/start touches, which is
// driven by compose file path exactly as the live path already does.
func dryRunContainerNames(composePath, serviceName string) []string {
	fallback := []string{"citadel-" + serviceName}
	data, err := os.ReadFile(composePath)
	if err != nil {
		return fallback
	}
	var doc struct {
		Services map[string]struct {
			ContainerName string `yaml:"container_name"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Services) == 0 {
		return fallback
	}
	names := make([]string, 0, len(doc.Services))
	for _, svc := range doc.Services {
		if svc.ContainerName != "" {
			names = append(names, svc.ContainerName)
		}
	}
	if len(names) == 0 {
		return fallback
	}
	sort.Strings(names)
	return names
}

// dispatchModuleAction runs the requested action against an injected
// liveModuleOps, kept separate from runModuleControl's manifest-validation so
// the stop/start/restart behavior (incl. restart's stop-then-start
// composition and the already-in-state no-op messaging) is unit-testable with
// a stubbed ops, without a container runtime or a real manifest on disk.
func dispatchModuleAction(ops *liveModuleOps, name string, action moduleAction) error {
	switch action {
	case moduleActionStop:
		return moduleControlStop(ops, name)
	case moduleActionStart:
		return moduleControlStart(ops, name)
	case moduleActionRestart:
		if err := moduleControlStop(ops, name); err != nil {
			return fmt.Errorf("restart %q: stop failed: %w", name, err)
		}
		if err := moduleControlStart(ops, name); err != nil {
			return fmt.Errorf("restart %q: start failed: %w", name, err)
		}
		fmt.Printf("✅ Module '%s' restarted.\n", name)
		return nil
	default:
		return fmt.Errorf("unknown action %v", action)
	}
}

// moduleControlStop stops a single module via the scoped liveModuleOps.Stop
// primitive. It ALWAYS calls ops.Stop -- never skips it based on the
// isRunning pre-check -- because that check (a live `docker inspect` by
// container name) fails closed to "not running" on a docker-unreachable node
// or a third-party module compose that names its container differently, and
// a skipped stop on a false "already stopped" reading would leave a real
// engine (and its VRAM) up while the CLI reports success. ops.Stop itself is
// idempotent (compose down on an already-down stack, or a missing compose
// file, is a no-op), so calling it unconditionally is safe; the pre-check is
// used ONLY to choose the "already stopped" vs. "stopping" message.
func moduleControlStop(ops *liveModuleOps, name string) error {
	alreadyStopped := !ops.isRunning(name)
	if alreadyStopped {
		fmt.Printf("ℹ️  Module '%s' is already stopped.\n", name)
	} else {
		fmt.Printf("--- 🛑 Stopping module: %s ---\n", name)
	}
	if err := ops.Stop(context.Background(), name); err != nil {
		return fmt.Errorf("stop %q: %w", name, err)
	}
	if !alreadyStopped {
		fmt.Printf("✅ Module '%s' stopped.\n", name)
	}
	return nil
}

// moduleControlStart starts a single module via the scoped liveModuleOps.Start
// primitive. Mirrors moduleControlStop: ALWAYS calls ops.Start (idempotent --
// startService inspects the container itself and no-ops if already running),
// using the isRunning pre-check only to choose the message, not to skip the
// call. See moduleControlStop for why skipping on that check would be unsafe.
func moduleControlStart(ops *liveModuleOps, name string) error {
	alreadyRunning := ops.isRunning(name)
	if alreadyRunning {
		fmt.Printf("ℹ️  Module '%s' is already running.\n", name)
	} else {
		fmt.Printf("--- 🚀 Starting module: %s ---\n", name)
	}
	if err := ops.Start(context.Background(), name); err != nil {
		return fmt.Errorf("start %q: %w", name, err)
	}
	if !alreadyRunning {
		fmt.Printf("✅ Module '%s' is up.\n", name)
	}
	return nil
}

// manifestServiceNames returns a sorted list of the manifest's service names,
// for the "unknown module" error hint.
func manifestServiceNames(manifest *CitadelManifest) []string {
	names := make([]string, 0, len(manifest.Services))
	for _, s := range manifest.Services {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

// lookupService returns the named service from the manifest. Callers must
// check hasService(manifest, name) first; a not-found name returns a zero
// Service.
func lookupService(manifest *CitadelManifest, name string) Service {
	for _, s := range manifest.Services {
		if s.Name == name {
			return s
		}
	}
	return Service{}
}
