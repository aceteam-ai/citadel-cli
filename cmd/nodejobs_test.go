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
		WorkflowExec: workflow.NewExecutor(workflow.ExecutorConfig{}),
		HandlerLog:   func(string, ...any) {},
	}

	// Build the base set and register the privileged handlers exactly as both
	// runWork and runTUIWorker do.
	handlers, _ := buildNodeJobHandlers(opts)
	runner := worker.NewRunner(nil, handlers, worker.RunnerConfig{})
	registerPrivilegedNodeJobHandlers(runner, opts)

	// The two node-targeted privileged types must be dispatchable.
	for _, jt := range []string{worker.JobTypeWhatsAppProvision, worker.JobTypeAgentUpdate} {
		if !runner.CanHandle(jt) {
			t.Errorf("node-job handler set does not cover %q; a control-center-only worker would fail it with 'no handler'", jt)
		}
	}

	// Sanity: the base legacy shell handler and the workflow handler are also present
	// so this remains the FULL set, not a privileged-only subset.
	if !runner.CanHandle(worker.JobTypeShellCommand) {
		t.Errorf("node-job handler set missing SHELL_COMMAND")
	}
	if !runner.CanHandle("WORKFLOW_RUN") {
		t.Errorf("node-job handler set missing WORKFLOW_RUN")
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
