package cmd

import (
	"context"
	"path/filepath"
	"runtime"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/network"
	"github.com/aceteam-ai/citadel-cli/internal/pairingdisplay"
	"github.com/aceteam-ai/citadel-cli/internal/status"
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
	Source worker.JobSource
	// OrgID is the organization chosen for this worker's per-node queue.
	OrgID string
	// WorkspaceDir is the sandbox root for file-operation handlers.
	WorkspaceDir string
	// ConfigDir is the citadel.yaml manifest directory (enables service handlers).
	// May be empty (the control center historically ran without it).
	ConfigDir string
	// PermissionsDir is the authoritative machine-level node configuration
	// directory. It must not depend on which user launched the worker.
	PermissionsDir string
	// AllowReadOutsideWorkspace lets read-only file handlers escape the sandbox.
	AllowReadOutsideWorkspace bool
	// ShellDisabled omits SHELL_COMMAND because it cannot execute while disabled.
	ShellDisabled bool
	// ShellEnabled reloads the persisted shell permission for each job so a
	// config push applies without a worker restart.
	ShellEnabled func() bool
	// ShellHasPasscode reports whether a node passcode is configured, so an enabled
	// shell can distinguish passcode_not_set from passcode_invalid. Required whenever
	// ShellDisabled is false: a nil signal makes shell fail closed. Wired from the
	// persisted node passcode.
	ShellHasPasscode func() bool
	// ShellVerifyPasscode gates an ENABLED SHELL_COMMAND handler on the per-node
	// passcode (aceteam#6524). Required whenever ShellDisabled is false: a nil
	// verifier makes shell fail closed. Wired from the persisted node passcode.
	ShellVerifyPasscode func(pin string) bool
	// DesktopDisabled skips registration of the screen/VNC/desktop handlers
	// (aceteam#6524). Wired from the persisted `desktop` node permission
	// (default-DENY on a fresh node).
	DesktopDisabled bool
	// DesktopEnabled reloads the persisted desktop permission for each routing
	// decision so configuration pushes take effect without a restart.
	DesktopEnabled func() bool
	// FilesDisabled skips registration of the file browse/host handlers
	// (aceteam#6524). Wired from the persisted `files` node permission
	// (default-DENY on a fresh node).
	FilesDisabled bool
	// FilesEnabled reloads the persisted files permission for each routing
	// decision so revocation immediately removes execution authority.
	FilesEnabled func() bool
	// LogFn routes legacy handler job output through a callback instead of stdout.
	LogFn func(level, msg string)
	// WorkflowExec backs the WORKFLOW_RUN handler. Required.
	WorkflowExec *workflow.Executor
	// HandlerLog is the plain logger the privileged handlers (AGENT_UPDATE,
	// WHATSAPP_PROVISION) use for their own progress lines. Required.
	HandlerLog func(format string, args ...any)
	// PinnedServices is the node's pinned_services allowlist (citadel #577). Used
	// by the model-hotswap swap manager (#632) so a pinned engine is never
	// evicted to swap in another. Optional.
	PinnedServices []string
	// InstanceEnabled is true only when this node has enabled Proxmox
	// provisioning configuration. INSTANCE_* is otherwise not dispatchable.
	InstanceEnabled bool
}

// buildNodeJobHandlers returns the base node-job handler set: the legacy Nexus
// handlers (shell, file ops, service management, inference, ...) plus the workflow
// handler. It is the initial handler slice passed to worker.NewRunner. The
// privileged, runner-coupled handlers (AGENT_UPDATE, WHATSAPP_PROVISION) are added
// afterward via registerPrivilegedNodeJobHandlers, because they need the live
// runner's Drain/ActiveJobs for the "publish result, THEN restart" ordering.
//
// The second return value is the model-hotswap swap manager attached to the
// llm_inference handler (nil under the break-glass disable, or no config dir) --
// it is constructed here, not at the heartbeat collector site, so callers that
// want to surface swap activity on the heartbeat (citadel-cli#717) must thread
// it through themselves; see cmd/work.go's nodeSwapManager/swapStatsFn.
func buildNodeJobHandlers(opts nodeJobHandlerOpts) ([]worker.JobHandler, *worker.SwapManager) {
	handlers := worker.CreateLegacyHandlersWithOpts(worker.LegacyHandlerOpts{
		LogFn:                     opts.LogFn,
		WorkspaceDir:              opts.WorkspaceDir,
		ConfigDir:                 opts.ConfigDir,
		PermissionsDir:            opts.PermissionsDir,
		AllowReadOutsideWorkspace: opts.AllowReadOutsideWorkspace,
		ShellDisabled:             opts.ShellDisabled,
		ShellEnabled:              opts.ShellEnabled,
		ShellHasPasscode:          opts.ShellHasPasscode,
		ShellVerifyPasscode:       opts.ShellVerifyPasscode,
		DesktopDisabled:           opts.DesktopDisabled,
		DesktopEnabled:            opts.DesktopEnabled,
		FilesDisabled:             opts.FilesDisabled,
		FilesEnabled:              opts.FilesEnabled,
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
	llmHandler := worker.NewLLMInferenceHandler()
	// Model hotswap (citadel-cli#632, default ON): unless CITADEL_MODEL_HOTSWAP is
	// explicitly disabled (0/false/no/off) and this node has a config dir, attach a
	// swap manager so an installed-but-not-resident target engine is swapped in on
	// demand. Returns nil (no swapper) only under the break-glass disable, in which
	// case the handler is byte-for-byte the pre-#632 one.
	swapper := newModelSwapManager(opts.ConfigDir, opts.WorkspaceDir, opts.PinnedServices, opts.HandlerLog)
	if swapper != nil {
		llmHandler = llmHandler.WithSwapper(swapper)
	}
	handlers = append(handlers, llmHandler)
	// document_rasterize (issue #675): render selected PDF pages to images so a
	// scanned document can reach an OCR model, which only accepts raster images.
	// Registered unconditionally alongside llm_inference: it needs no workspace
	// or config, and the caller renders and then OCRs on the SAME node, so a
	// control-center-only node must answer both or the pair splits.
	handlers = append(handlers, worker.NewDocumentRasterizeHandler(worker.DocumentRasterizeConfig{
		Log: opts.HandlerLog,
	}))
	// SHOW_PAIRING_CODE / CLEAR_PAIRING_CODE (issue #659 P0): render a
	// platform-pushed node:exec pairing code on this node's active text
	// console. Registered unconditionally alongside llm_inference/
	// document_rasterize -- it needs no workspace or config, and delegates
	// all state/rendering to the pairingdisplay.Get() process-wide singleton
	// (configured with the machine-convergent state dir by runWork/
	// runTUIWorker; see docs/design-pairing-display.md §12). Ops.Log NEVER
	// receives the code -- see internal/worker/pairing_display.go's package
	// doc for the invariant this call site must preserve.
	handlers = append(handlers, worker.NewPairingDisplayHandler(worker.PairingDisplayConfig{
		Ops: pairingdisplay.Get(),
		Log: opts.HandlerLog,
	}))
	return handlers, swapper
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
	// Both direct Redis and API-proxy workers use the platform's canonical
	// fine-tune hash and cancel key. API mode requires the scoped companion
	// endpoint; it never falls back to the generic KV route.
	var control worker.FineTuneControl
	switch src := opts.Source.(type) {
	case *worker.RedisSource:
		control = worker.NewRedisFineTuneControl(src, opts.OrgID, runner.NodeID())
	case *worker.APISource:
		control = worker.NewAPIFineTuneControl(src)
	}
	if control != nil && runtime.GOOS == "linux" && opts.ConfigDir != "" && opts.WorkspaceDir != "" {
		service := jobs.NewServiceHandlerWithWorkspace(opts.ConfigDir, opts.WorkspaceDir)
		runner.RegisterHandler(worker.NewFineTuneHandler(worker.FineTuneConfig{
			NodeID: runner.NodeID(), WorkspaceDir: opts.WorkspaceDir,
			OutputRoot:  filepath.Join(opts.ConfigDir, "finetune", "adapters"),
			SafetyDir:   filepath.Join(opts.ConfigDir, "finetune", "safety"),
			CacheDir:    filepath.Join(opts.ConfigDir, "finetune", "cache"),
			Image:       "citadel-finetune:local",
			Control:     control,
			Reservation: &fineTuneServiceReservation{service: service},
		}))
	}
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
		// Admin-key rotation (citadel#624 part 3): the platform-triggerable half of
		// the `citadel whatsapp rotate-key` CLI, honored when the payload sets
		// rotate_admin_key: true. Same rotation primitive, wired with this node's
		// real docker/network edges via the shared whatsappRotateDeps.
		Rotate: func(ctx context.Context) (*whatsapp.RotateResult, error) {
			return whatsapp.RotateAdminKey(ctx, whatsappRotateDeps(opts.HandlerLog))
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
		External: &worker.ExternalModuleConfig{
			NodeID:             runner.NodeID(),
			OrgID:              opts.OrgID,
			Dir:                network.GetNodeConfigDir(),
			Ops:                newLiveModuleOps(opts.HandlerLog),
			ManagedVLLMRunning: status.ManagedVLLMContainerRunning,
			Drain:              runner.Drain,
			Resume:             runner.Resume,
			ActiveJobs:         runner.ActiveJobs,
			Log:                opts.HandlerLog,
		},
	}))

	// EXPOSE_SET (issue #598): expose a local node service on the gateway with
	// private/org/link visibility and return the managed mesh URL. The live ops
	// adapter programs the in-process gateway; the node half of the `expose` MCP
	// verb's contract.
	runner.RegisterHandler(worker.NewExposeSetHandler(worker.ExposeSetConfig{
		Ops: liveExposeOps{},
		Log: opts.HandlerLog,
	}))

	// EXPOSE_LIST + UNEXPOSE (issue #944): remote inventory read-back and remote
	// teardown, the two custody gaps EXPOSE_SET alone left open. Same live ops
	// adapter as EXPOSE_SET (one funnel, one mutex — see cmd/expose_ops.go).
	runner.RegisterHandler(worker.NewExposeListHandler(worker.ExposeListConfig{
		Ops: liveExposeOps{},
		Log: opts.HandlerLog,
	}))
	runner.RegisterHandler(worker.NewUnexposeHandler(worker.UnexposeConfig{
		Ops: liveExposeOps{},
		Log: opts.HandlerLog,
	}))

	// INSTANCE_* is a mutating hypervisor capability: only register it when the
	// required Proxmox configuration is enabled. A node without that config must
	// not advertise work it will refuse.
	if opts.InstanceEnabled {
		runner.RegisterHandler(worker.NewInstanceHandler(worker.InstanceHandlerConfig{
			Provider: newInstanceProviderFactory(opts.ConfigDir, opts.HandlerLog),
			Log:      opts.HandlerLog,
		}))
	}
}

type fineTuneServiceReservation struct{ service *jobs.ServiceHandler }

func (r *fineTuneServiceReservation) Reserve(ctx context.Context, jobID string) ([]string, error) {
	res, err := r.service.ReserveNamed(jobs.JobContext{Ctx: ctx}, jobID, []string{"unlimited-ocr", "ollama"})
	if res == nil {
		return nil, err
	}
	return res.Evicted, err
}

func (r *fineTuneServiceReservation) Release(ctx context.Context, jobID string) error {
	_, err := r.service.Release(jobs.JobContext{Ctx: ctx}, jobID)
	return err
}

// nodeJobOrgID mirrors the per-node queue builder's device-first org choice.
func nodeJobOrgID() string {
	if device := getDeviceConfigFromFile(); device != nil && device.OrgID != "" {
		return device.OrgID
	}
	if manifest, _, err := findAndReadManifest(); err == nil {
		return manifest.Node.OrgID
	}
	return ""
}
