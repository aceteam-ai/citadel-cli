package cmd

import (
	"context"

	"github.com/aceteam-ai/citadel-cli/internal/whatsapp"
	"github.com/aceteam-ai/citadel-cli/internal/worker"
	"github.com/aceteam-ai/citadel-cli/internal/workflow"
)

// nodeJobHandlerOpts bundles the node/environment edges the node-job handler set
// needs. It is shared by BOTH `citadel work` (runWork) and the control-center TUI
// (runTUIWorker) so a control-center-only node handles the exact same node-job set
// as the dedicated worker — no more "node vX has no handler for WHATSAPP_PROVISION"
// when only the control center runs (the competing-consumer incident).
type nodeJobHandlerOpts struct {
	// WorkspaceDir is the sandbox root for file-operation handlers.
	WorkspaceDir string
	// ConfigDir is the citadel.yaml manifest directory (enables service handlers).
	// May be empty (the control center historically ran without it).
	ConfigDir string
	// AllowReadOutsideWorkspace lets read-only file handlers escape the sandbox.
	AllowReadOutsideWorkspace bool
	// ShellDisabled registers SHELL_COMMAND in a refusing state (still dispatchable).
	ShellDisabled bool
	// DesktopDisabled skips registration of the screen/VNC/desktop handlers
	// (aceteam#6524). Wired from the persisted `desktop` node permission
	// (default-DENY on a fresh node).
	DesktopDisabled bool
	// FilesDisabled skips registration of the file browse/host handlers
	// (aceteam#6524). Wired from the persisted `files` node permission
	// (default-DENY on a fresh node).
	FilesDisabled bool
	// LogFn routes legacy handler job output through a callback instead of stdout.
	LogFn func(level, msg string)
	// WorkflowExec backs the WORKFLOW_RUN handler. Required.
	WorkflowExec *workflow.Executor
	// HandlerLog is the plain logger the privileged handlers (AGENT_UPDATE,
	// WHATSAPP_PROVISION) use for their own progress lines. Required.
	HandlerLog func(format string, args ...any)
}

// buildNodeJobHandlers returns the base node-job handler set: the legacy Nexus
// handlers (shell, file ops, service management, inference, ...) plus the workflow
// handler. It is the initial handler slice passed to worker.NewRunner. The
// privileged, runner-coupled handlers (AGENT_UPDATE, WHATSAPP_PROVISION) are added
// afterward via registerPrivilegedNodeJobHandlers, because they need the live
// runner's Drain/ActiveJobs for the "publish result, THEN restart" ordering.
func buildNodeJobHandlers(opts nodeJobHandlerOpts) []worker.JobHandler {
	handlers := worker.CreateLegacyHandlersWithOpts(worker.LegacyHandlerOpts{
		LogFn:                     opts.LogFn,
		WorkspaceDir:              opts.WorkspaceDir,
		ConfigDir:                 opts.ConfigDir,
		AllowReadOutsideWorkspace: opts.AllowReadOutsideWorkspace,
		ShellDisabled:             opts.ShellDisabled,
		DesktopDisabled:           opts.DesktopDisabled,
		FilesDisabled:             opts.FilesDisabled,
	})
	if opts.WorkflowExec != nil {
		handlers = append(handlers, workflow.NewHandler(opts.WorkflowExec))
	}
	// llm_inference (issue #590): the aceteam python-backend dispatches
	// job_type="llm_inference" for ALL fabric inference (OpenAI gateway, /fabric
	// model deploys, mesh chat). Registered unconditionally — like the workflow
	// handler it needs no workspace/config — so both `citadel work` and the
	// control-center worker route it to the node-local engine (vllm/sglang/
	// ollama/llamacpp/bonsai). Without it every inference job failed with
	// "unsupported job type \"llm_inference\": node X has no handler for it".
	handlers = append(handlers, worker.NewLLMInferenceHandler())
	return handlers
}

// registerPrivilegedNodeJobHandlers registers the node-targeted privileged handlers
// (AGENT_UPDATE and WHATSAPP_PROVISION) onto an already-constructed runner. These
// are the handlers whose absence caused the competing-consumer incident: they were
// registered only in `citadel work`, so when the control center ran its own worker
// beside the real one and grabbed a WHATSAPP_PROVISION/AGENT_UPDATE job off the
// shared per-node stream, it failed the job with "no handler" even though the real
// worker's binary could handle it.
//
// They are registered after the runner exists so AGENT_UPDATE can borrow the
// runner's Drain/ActiveJobs to drain in-flight work before a self-restart.
func registerPrivilegedNodeJobHandlers(runner *worker.Runner, opts nodeJobHandlerOpts) {
	// AGENT_UPDATE (aceteam#4427): remote agent update + restart for this node.
	runner.RegisterHandler(worker.NewAgentUpdateHandler(worker.AgentUpdateConfig{
		Version:    Version,
		Drain:      func() { runner.Drain() },
		ActiveJobs: runner.ActiveJobs,
		Log:        opts.HandlerLog,
	}))

	// WHATSAPP_PROVISION (aceteam#4454): remote-provision the Baileys bridge on the
	// user's own node. Reuses the same `citadel whatsapp up` orchestration wired with
	// this node's real docker/git/mesh edges.
	runner.RegisterHandler(worker.NewWhatsAppProvisionHandler(worker.WhatsAppProvisionConfig{
		Provision: func(ctx context.Context, req whatsapp.ProvisionRequest) (*whatsapp.ProvisionResult, error) {
			return whatsapp.Provision(ctx, req, whatsappProvisionDeps(defaultWhatsAppSource, ""))
		},
		Log: opts.HandlerLog,
	}))

	// MODULE_SET (aceteam#5280, interim): imperatively apply a single module's
	// desired state (running/stopped/absent) on this node, reusing the tested
	// reconcile engine scoped to one module and the live catalog/compose/lockfile
	// adapter. Converges into the durable pull-based desired-state loop (#4273).
	runner.RegisterHandler(worker.NewModuleSetHandler(worker.ModuleSetConfig{
		Ops: newLiveModuleOps(opts.HandlerLog),
		Log: opts.HandlerLog,
	}))

	// INSTANCE_* (aceteam#5963): fabric instance provisioning on this node's
	// Proxmox hypervisor. Registered unconditionally so nodes without a proxmox
	// provisioning config fail these jobs with a clear message; the lazy factory
	// gates on proxmox.json's provisioning.enabled.
	runner.RegisterHandler(worker.NewInstanceHandler(worker.InstanceHandlerConfig{
		Provider: newInstanceProviderFactory(opts.ConfigDir, opts.HandlerLog),
		Log:      opts.HandlerLog,
	}))
}
