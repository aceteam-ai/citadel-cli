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
	handlers := buildNodeJobHandlers(opts)
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
