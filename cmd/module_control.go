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
	"sort"
	"strings"

	"github.com/spf13/cobra"
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

Already-stopped is a clean no-op success.`,
	Example: `  citadel module stop vllm`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleControl(args[0], moduleActionStop)
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

Already-running is a clean no-op success.`,
	Example: `  citadel module start vllm`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleControl(args[0], moduleActionStart)
	},
	SilenceUsage: true,
}

var moduleRestartCmd = &cobra.Command{
	Use:   "restart <name>",
	Short: "Restart a single module: stop then start it (scoped)",
	Long: `Restarts one module by name -- equivalent to 'citadel module stop <name>'
immediately followed by 'citadel module start <name>'. No other module is
touched and the worker is not restarted.`,
	Example: `  citadel module restart vllm`,
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runModuleControl(args[0], moduleActionRestart)
	},
	SilenceUsage: true,
}

func init() {
	moduleCmd.AddCommand(moduleStopCmd)
	moduleCmd.AddCommand(moduleStartCmd)
	moduleCmd.AddCommand(moduleRestartCmd)
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
// existing single-service scope), then dispatches to the scoped
// stop/start primitive. Kept separate from the cobra RunE closures so it is
// unit-testable without going through Cobra.
func runModuleControl(name string, action moduleAction) error {
	manifest, _, err := findAndReadManifest()
	if err != nil {
		return fmt.Errorf("%w\nHint: run 'citadel init' first, or 'citadel module install <source>' to install a module", err)
	}

	if !hasService(manifest, name) {
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

	log := func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	ops := newLiveModuleOps(log)
	return dispatchModuleAction(ops, name, action)
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
