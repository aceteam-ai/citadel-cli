package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/internal/workflow"
)

// TestNodeJobHandlersCoverPrivilegedTypes pins the fix for the competing-consumer
// incident: the shared node-job handler set (used by BOTH `citadel work` and the
// control-center-only worker) must be able to handle the node-targeted privileged
// job types WHATSAPP_PROVISION and AGENT_UPDATE, not just the legacy shell/file set.
// A control-center-only node that lacks these would fail such jobs with
// "node vX has no handler", the exact bug this change removes.
func TestNodeJobHandlersCoverPrivilegedTypes(t *testing.T) {
	opts := nodeJobHandlerOpts{
		WorkspaceDir: t.TempDir(),
		// A fresh node has no persisted shell opt-in, so the production callers
		// pass ShellDisabled=true. Keep this fixture aligned with that default-deny
		// configuration rather than relying on nodeJobHandlerOpts' zero value.
		ShellDisabled: true,
		WorkflowExec:  workflow.NewExecutor(workflow.ExecutorConfig{}),
		HandlerLog:    func(string, ...any) {},
	}

	// Build the base set and register the privileged handlers exactly as both
	// runWork and runTUIWorker do.
	handlers, _ := buildNodeJobHandlers(opts)
	runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
	registerPrivilegedNodeJobHandlers(runner, opts)

	// The node-targeted privileged types must be dispatchable in both workers.
	for _, jt := range []string{worker.JobTypeWhatsAppProvision, worker.JobTypeAgentUpdate, worker.JobTypeWorkerControl} {
		if !runner.CanHandle(jt) {
			t.Errorf("node-job handler set does not cover %q; a control-center-only worker would fail it with 'no handler'", jt)
		}
	}

	// Shell is default-deny, so this unconfigured node must not advertise a
	// handler which will refuse every command.
	if runner.CanHandle(worker.JobTypeShellCommand) {
		t.Errorf("unconfigured node must not advertise SHELL_COMMAND")
	}
	if !runner.CanHandle("WORKFLOW_RUN") {
		t.Errorf("node-job handler set missing WORKFLOW_RUN")
	}
}

// TestNodeJobHandlersAdvertiseReadOnlyInspectionWithShellDisabled pins the
// recovery posture from aceteam#9962: disabling remote shell must not remove
// separately-authorized, cross-platform read-only inspection handlers.
func TestNodeJobHandlersAdvertiseReadOnlyInspectionWithShellDisabled(t *testing.T) {
	opts := nodeJobHandlerOpts{
		WorkspaceDir:  t.TempDir(),
		ConfigDir:     t.TempDir(),
		ShellDisabled: true,
		WorkflowExec:  workflow.NewExecutor(workflow.ExecutorConfig{}),
		HandlerLog:    func(string, ...any) {},
	}
	handlers, _ := buildNodeJobHandlers(opts)
	runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
	registerPrivilegedNodeJobHandlers(runner, opts)

	supported := make(map[string]bool)
	for _, jobType := range runner.SupportedJobTypes() {
		supported[jobType] = true
	}
	for _, jobType := range []string{
		worker.JobTypeFileRead,
		worker.JobTypeFileList,
		worker.JobTypeFileSearch,
		worker.JobTypeServiceStatus,
	} {
		if !supported[jobType] {
			t.Errorf("read-only inspection type %q missing with shell disabled", jobType)
		}
	}
	if supported[worker.JobTypeShellCommand] {
		t.Fatal("disabled SHELL_COMMAND must not be advertised")
	}
}

func TestNodeJobHandlersExcludeOffPlatformIOSAndUnconfiguredInstances(t *testing.T) {
	opts := nodeJobHandlerOpts{
		WorkspaceDir: t.TempDir(), ConfigDir: t.TempDir(), ShellDisabled: true,
		WorkflowExec: workflow.NewExecutor(workflow.ExecutorConfig{}), HandlerLog: func(string, ...any) {},
	}
	handlers, _ := buildNodeJobHandlers(opts)
	runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
	registerPrivilegedNodeJobHandlers(runner, opts)
	if runner.CanHandle(worker.JobTypeInstanceProvision) {
		t.Fatal("INSTANCE_* must not be advertised without enabled Proxmox configuration")
	}

	// Exercise the platform gate without depending on the OS that runs tests.
	legacy := worker.CreateLegacyHandlersWithOpts(worker.LegacyHandlerOpts{GOOS: "linux"})
	iosRunner := worker.NewRunner(nil, legacy, worker.RunnerConfig{})
	if iosRunner.CanHandle(worker.JobTypeIOSBuild) {
		t.Fatal("IOS_BUILD must not be advertised off macOS")
	}
}

// TestBuildNodeJobHandlersReturnsSwapManager pins the citadel-cli#717 threading
// contract: buildNodeJobHandlers must hand back the swap manager it constructs
// internally (not just the wrapped llm_inference handler), because the caller
// needs it to wire swap activity onto the heartbeat (cmd/work.go's
// nodeSwapManager/swapStatsFn). It is non-nil exactly when hotswap is enabled
// (the default) AND a config dir was supplied, and nil under the break-glass
// disable -- matching newModelSwapManager's own contract.
func TestBuildNodeJobHandlersReturnsSwapManager(t *testing.T) {
	t.Run("no config dir: no swap manager (matches pre-existing behavior)", func(t *testing.T) {
		opts := nodeJobHandlerOpts{
			WorkspaceDir: t.TempDir(),
			WorkflowExec: workflow.NewExecutor(workflow.ExecutorConfig{}),
			HandlerLog:   func(string, ...any) {},
		}
		_, swapper := buildNodeJobHandlers(opts)
		if swapper != nil {
			t.Errorf("swapper = %v, want nil with no ConfigDir", swapper)
		}
	})

	t.Run("hotswap enabled (default) with config dir: swap manager attached", func(t *testing.T) {
		opts := nodeJobHandlerOpts{
			WorkspaceDir: t.TempDir(),
			ConfigDir:    t.TempDir(),
			WorkflowExec: workflow.NewExecutor(workflow.ExecutorConfig{}),
			HandlerLog:   func(string, ...any) {},
		}
		_, swapper := buildNodeJobHandlers(opts)
		if swapper == nil {
			t.Fatal("swapper = nil, want a swap manager when hotswap is enabled (default) and ConfigDir is set")
		}
		// SwapStats() must be safe to call immediately with no swaps recorded yet
		// -- this is exactly what the heartbeat closure does on every collection.
		stats := swapper.SwapStats()
		if stats.SwapsPerHour != 0 || len(stats.Recent) != 0 {
			t.Errorf("SwapStats() = %+v, want zero-value stats before any swap", stats)
		}
	})

	t.Run("break-glass disabled: no swap manager even with config dir", func(t *testing.T) {
		t.Setenv("CITADEL_MODEL_HOTSWAP", "false")
		opts := nodeJobHandlerOpts{
			WorkspaceDir: t.TempDir(),
			ConfigDir:    t.TempDir(),
			WorkflowExec: workflow.NewExecutor(workflow.ExecutorConfig{}),
			HandlerLog:   func(string, ...any) {},
		}
		_, swapper := buildNodeJobHandlers(opts)
		if swapper != nil {
			t.Errorf("swapper = %v, want nil under CITADEL_MODEL_HOTSWAP=false", swapper)
		}
	})
}
