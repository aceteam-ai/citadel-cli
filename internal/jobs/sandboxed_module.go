// internal/jobs/sandboxed_module.go
//
// Node-side plumbing for the generic sandboxed-module runtime
// (aceteam-ai/citadel-cli#1288). The compose CONTRACT lives in
// services.RenderSandboxedModuleCompose (a leaf, pure); this file adds the
// pieces that need the container-runtime seam (internal/catalog) and the job
// lifecycle (internal/jobs):
//
//   - ErrSandboxedModuleUnavailable: the typed, terminal dispatch-refusal error
//     a consumer returns when the runtime (or the module's image) is missing,
//     mirroring jobs.ErrFilesDisabled / jobs.ErrReceiptSigningUnavailable. The
//     worker runner treats it as terminal (no retry) -- see runner.go.
//   - ProbeSandboxedRuntime / EnsureSandboxedModuleAvailable: the startup/dispatch
//     availability probe. ONE decision feeds both the heartbeat capability
//     (cmd wires it into the status collector) and the dispatch guard, so they
//     can never disagree.
//   - SandboxedModuleCapabilities: the #1287 JobCapabilities PRESET a sandboxed
//     helper handler DECLARES (heavy lane + long deadline + cooperative cancel),
//     so the resource posture composes with the generic lifecycle contract
//     instead of a runner-side allowlist.
package jobs

import (
	"context"
	"errors"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/services"
)

// ErrSandboxedModuleUnavailable is the terminal, typed refusal for a job that
// needs a sandboxed helper module this node cannot run: no container runtime
// that can enforce the profile's cgroup limits, the runtime is unreachable, or
// the module's image is not present locally. A sandboxed module runs with NO
// network and therefore cannot pull its own image, so a missing image is
// terminal: a retry cannot fix it. The JSON body matches the shape of the other
// node-permission sentinels so the control plane renders a consistent reason.
var ErrSandboxedModuleUnavailable = errors.New(`{"reason":"sandboxed_module_unavailable","message":"This node cannot run the required sandboxed helper module. Either no container runtime can enforce the sandbox cgroup limits (rootless Podman needs cpu/memory/pids controller delegation; run 'citadel doctor'), the runtime is not reachable, or the module image is not present locally. A sandboxed module runs with no network and cannot pull its own image -- pre-pull it on the node, then retry."}`)

// SandboxedRuntimeStatus is the generic (per-NODE, not per-module) availability
// of the sandboxed runtime, reported on the heartbeat so a dispatcher routes
// sandboxed work only to capable nodes.
type SandboxedRuntimeStatus struct {
	// Available is true when a container runtime is selected, reachable, and can
	// enforce the profile's cgroup limits.
	Available bool
	// Reason explains a false Available (empty when Available is true).
	Reason string
}

// SandboxedModuleCapabilities is the generic #1287 lifecycle preset for a
// sandboxed helper handler (document extraction, OCR, media probing, archive
// unpacking). A handler for such a job type RETURNS this from its
// Capabilities()/DeclaredCapabilities so the runner routes it onto the heavy
// execution lane with the long watchdog tier and the cooperative mid-job cancel
// watch -- the same posture FILE_INDEX declares -- without any runner-side
// allowlist. A consumer that also meters the work (e.g. #1264's extractor)
// layers its own WorkActions/ReceiptOwner on top of these fields.
func SandboxedModuleCapabilities() JobCapabilities {
	return JobCapabilities{
		CooperativeCancel: true,
		Lane:              LaneHeavy,
		Deadline:          DeadlineLong,
	}
}

// sandboxRuntimeDeps are the injectable seams that let the probe be unit-tested
// without a real container runtime.
type sandboxRuntimeDeps struct {
	selectRuntime func() catalog.ContainerRuntime
	preflight     func(rt catalog.ContainerRuntime) error
	engineReady   func(ctx context.Context, rt catalog.ContainerRuntime) error
	imagePresent  func(ctx context.Context, rt catalog.ContainerRuntime, ref string) bool
}

func defaultSandboxRuntimeDeps() sandboxRuntimeDeps {
	return sandboxRuntimeDeps{
		selectRuntime: catalog.SelectContainerRuntime,
		preflight: func(rt catalog.ContainerRuntime) error {
			// The profile always declares cpu/memory/pids limits; a rootless
			// Podman node that cannot enforce them must report unavailable rather
			// than silently run unlimited. Docker/rootful returns nil here.
			return rt.PreflightLimitControllers("cpu", "memory", "pids")
		},
		engineReady: func(ctx context.Context, rt catalog.ContainerRuntime) error {
			return rt.Info(ctx).Run()
		},
		imagePresent: func(ctx context.Context, rt catalog.ContainerRuntime, ref string) bool {
			return rt.EngineCommandContext(ctx, "image", "inspect", ref).Run() == nil
		},
	}
}

func probeSandboxedRuntime(ctx context.Context, d sandboxRuntimeDeps) SandboxedRuntimeStatus {
	rt := d.selectRuntime()
	if err := d.preflight(rt); err != nil {
		return SandboxedRuntimeStatus{Reason: "container runtime cannot enforce sandbox cgroup limits: " + err.Error()}
	}
	if err := d.engineReady(ctx, rt); err != nil {
		return SandboxedRuntimeStatus{Reason: "container runtime not reachable: " + err.Error()}
	}
	return SandboxedRuntimeStatus{Available: true}
}

// ProbeSandboxedRuntime reports whether this node can run ANY sandboxed module:
// a container runtime is selected, reachable, and able to enforce the profile's
// cgroup limits. It is the single generic signal the heartbeat advertises
// (cmd projects it into status.SandboxedModuleCapability) and the first half of
// the per-module dispatch guard below.
func ProbeSandboxedRuntime(ctx context.Context) SandboxedRuntimeStatus {
	return probeSandboxedRuntime(ctx, defaultSandboxRuntimeDeps())
}

// EnsureSandboxedModuleAvailable is the dispatch-time guard a handler calls
// before running a job on a sandboxed module. It returns
// ErrSandboxedModuleUnavailable (terminal, no retry) when the runtime is
// unavailable OR the module's image is not present locally. On success the
// caller may render + start the profile via the runtime seam.
func EnsureSandboxedModuleAvailable(ctx context.Context, spec services.SandboxedModuleSpec) error {
	return ensureSandboxedModuleAvailable(ctx, spec, defaultSandboxRuntimeDeps())
}

func ensureSandboxedModuleAvailable(ctx context.Context, spec services.SandboxedModuleSpec, d sandboxRuntimeDeps) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if st := probeSandboxedRuntime(ctx, d); !st.Available {
		return ErrSandboxedModuleUnavailable
	}
	rt := d.selectRuntime()
	if !d.imagePresent(ctx, rt, spec.Image) {
		return ErrSandboxedModuleUnavailable
	}
	return nil
}
