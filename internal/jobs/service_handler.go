// internal/jobs/service_handler.go
package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/internal/status"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// serviceManifest is a minimal subset of the citadel.yaml manifest used by
// the service handler.  It lives here (not in cmd/) to avoid import cycles.
type serviceManifest struct {
	Services []manifestService `yaml:"services"`
	// PinnedServices is the node-wide allowlist of services that must NEVER be
	// preempted to make room for another deploy (citadel-cli#577). Mirrors
	// cmd/manifest.go CitadelManifest.PinnedServices.
	PinnedServices []string `yaml:"pinned_services,omitempty"`
}

// pinnedSet returns the pinned_services allowlist as a set for O(1) lookup,
// trimming blank entries.
func (m *serviceManifest) pinnedSet() map[string]bool {
	set := make(map[string]bool, len(m.PinnedServices))
	for _, n := range m.PinnedServices {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = true
		}
	}
	return set
}

type manifestService struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type,omitempty"`
	ComposeFile string `yaml:"compose_file,omitempty"`
	Port        int    `yaml:"port,omitempty"`
	// DesiredStatus mirrors cmd/manifest.go Service.DesiredStatus: "stopped"
	// makes an operator stop durable (the boot-time start paths skip the
	// service). Read here so handlers can reason about it; written via
	// setDesiredStatusInManifestFile (yaml.Node surgery, not this struct).
	DesiredStatus string `yaml:"desired_status,omitempty"`
	// EvictedByJob / EvictedPriorStatus mirror cmd/manifest.go Service's
	// citadel-cli#832 reservation markers (see there for the full explanation).
	// Written via setEvictedMarkersInManifestFile (yaml.Node surgery, not this
	// struct) by Reserve/Release/ReconcileOrphanedReservations (reservation.go).
	EvictedByJob       string `yaml:"evicted_by_job,omitempty"`
	EvictedPriorStatus string `yaml:"evicted_prior_status,omitempty"`
}

// ServiceHandler manages start/stop/status of services declared in the node's
// citadel.yaml manifest.  The job type (SERVICE_START, SERVICE_STOP,
// SERVICE_STATUS) is read from the incoming job's Type field.
type ServiceHandler struct {
	// ConfigDir is the absolute path to the directory containing citadel.yaml.
	ConfigDir string
	// WorkspaceDir is the absolute node workspace root. It is exported to
	// docker compose as CITADEL_WORKSPACE so compose files that bind-mount the
	// workspace (e.g. the transcribe sidecar) resolve to an absolute path even
	// when the worker was started without CITADEL_WORKSPACE in its environment.
	WorkspaceDir string
	// instances is the registry of payload-launched agent-runtime instances
	// (BYOC, citadel-cli#462), lazily initialized. These live outside
	// citadel.yaml, so SERVICE_STOP / SERVICE_STATUS find them here.
	instances *instanceStore

	// collectStatus, when non-nil, overrides live node-status collection used by
	// VRAM-aware preemption/reservation (#577, #832: preemptForVRAM, Reserve).
	// Tests inject a synthetic *status.NodeStatus to exercise eviction decisions
	// without shelling to docker/nvidia-smi. nil (the production default) uses
	// status.NewCollector(CollectorConfig{ConfigDir: h.ConfigDir}).Collect().
	collectStatus func() (*status.NodeStatus, error)
	// stopServiceFn / startServiceFn, when non-nil, override the reservation
	// primitive's evict/restore execution (#832: Reserve/Release) for tests, so
	// eviction ordering and marker correctness are verifiable without a live
	// docker daemon. nil (the production default) uses the real
	// StopServiceByName / StartServiceByName.
	stopServiceFn  func(name string) error
	startServiceFn func(name string) error
	// writeManifestFn, when non-nil, overrides the final write in every
	// yaml.Node-surgery manifest setter (addServiceToManifestFile,
	// setDesiredStatusInManifestFile, setEvictedMarkersInManifestFile). Tests
	// use it to simulate a manifest write failing partway through a multi-write
	// sequence (e.g. Release's desired_status-then-tag-clear pair, #832) without
	// needing a real disk-full/IO-error condition. nil (the production default)
	// uses os.WriteFile.
	writeManifestFn func(path string, data []byte) error
}

// NewServiceHandler creates a ServiceHandler rooted at configDir.
func NewServiceHandler(configDir string) *ServiceHandler {
	return &ServiceHandler{ConfigDir: configDir}
}

// NewServiceHandlerWithWorkspace creates a ServiceHandler that also knows the
// node workspace, so workspace-mounting compose services can be started.
func NewServiceHandlerWithWorkspace(configDir, workspaceDir string) *ServiceHandler {
	return &ServiceHandler{ConfigDir: configDir, WorkspaceDir: workspaceDir}
}

// serviceResult is the JSON structure returned for all service operations.
type serviceResult struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Kind    string `json:"kind"` // "docker" or "native"
	Error   string `json:"error,omitempty"`
	Action  string `json:"action,omitempty"`  // "start", "stop", "status"
	Message string `json:"message,omitempty"` // human-readable summary
	// Endpoint is the reachable host endpoint of a started docker service,
	// e.g. "127.0.0.1:7861". It is derived from the container's published port
	// bindings after `docker compose up` so the caller knows where to reach the
	// provisioned service. Empty for native services or when no host port is
	// published. See citadel-cli#415.
	Endpoint string `json:"endpoint,omitempty"`
	// Runtime is the docker container runtime a payload-launched instance runs
	// under (e.g. "kata", "runsc"). Empty for the daemon default (runc) and for
	// manifest/native services. See citadel-cli#470.
	Runtime string `json:"runtime,omitempty"`
}

func (h *ServiceHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	svcName := job.Payload["service"]
	if svcName == "" {
		return nil, fmt.Errorf("job payload missing 'service' field")
	}

	ctx.Log("info", "     - [Job %s] Service %s: %s", job.ID, job.Type, svcName)

	// Extended-payload launch path (BYOC agent runtimes, citadel-cli#462). A
	// SERVICE_START that carries an inline spec (image/env/host_port/volume) is
	// launched from the payload; it does not exist in citadel.yaml or the
	// embedded ServiceMap. The name-based manifest path below is left untouched
	// for embedded services.
	if job.Type == "SERVICE_START" && payloadHasInlineSpec(job.Payload) {
		return h.serviceStartPayload(ctx, job)
	}

	// SERVICE_STOP / SERVICE_STATUS for a previously payload-launched instance:
	// it is in neither the manifest nor the embedded map, so resolve it from the
	// instance store before falling through to the manifest path.
	if job.Type == "SERVICE_STOP" || job.Type == "SERVICE_STATUS" {
		if rec, ok, sErr := h.instanceStore().Get(svcName); sErr != nil {
			ctx.Log("warning", "     - [Job %s] instance store read failed: %v", job.ID, sErr)
		} else if ok {
			switch job.Type {
			case "SERVICE_STATUS":
				return h.serviceStatusPayload(rec)
			case "SERVICE_STOP":
				return h.serviceStopPayload(ctx, rec)
			}
		}
	}

	// Load manifest and validate service name against it.
	manifest, err := h.loadManifest()
	if err != nil {
		return nil, fmt.Errorf("failed to load manifest: %w", err)
	}

	svc, ok := h.findService(manifest, svcName)
	if !ok {
		// The manifest may predate a newly-embedded service (e.g. a node
		// initialized before "diffusers" existed, then binary-upgraded). The
		// heartbeat advertises every embedded ServiceMap key as available, so a
		// deploy can legitimately target a service the runtime manifest never
		// listed. Reconcile lazily: if the requested service is present in the
		// embedded ServiceMap, materialize its compose file and additively
		// register it in citadel.yaml, then proceed. This keeps
		// advertised == runnable without auto-starting every embedded service at
		// boot (which additively pre-populating the manifest would cause).
		// See citadel-cli#413.
		if _, embedded := embeddedservices.ServiceMap[svcName]; !embedded {
			return nil, fmt.Errorf("service %q not found in manifest (known: %s)",
				svcName, h.knownServiceNames(manifest))
		}
		var mErr error
		svc, mErr = h.materializeEmbeddedService(svcName)
		if mErr != nil {
			return nil, fmt.Errorf("failed to reconcile embedded service %q: %w", svcName, mErr)
		}
		ctx.Log("info", "     - [Job %s] Reconciled embedded service %s into manifest", job.ID, svcName)
	}

	switch job.Type {
	case "SERVICE_STATUS":
		return h.serviceStatus(svc)
	case "SERVICE_START":
		// An explicit remote start clears the durable stopped marker (mirrors
		// liveModuleOps.Start) so the service also starts on the next boot.
		// Cleared FIRST so a transiently-failed start still records the
		// operator's run intent. Best-effort: never blocks the start.
		if err := h.setDesiredStatusInManifestFile(svc.Name, ""); err != nil {
			ctx.Log("warning", "     - [Job %s] could not clear stopped marker for %s: %v", job.ID, svc.Name, err)
		}
		// Also clear any job-scoped reservation eviction tag (#832): an explicit
		// operator/platform start is a stronger signal than a pending
		// reservation restore, and clearing it here keeps a later Release for
		// the (now irrelevant) reserving job a harmless no-op instead of an
		// unexpected extra restart. Best-effort, same as above.
		if err := h.setEvictedMarkersInManifestFile(svc.Name, "", ""); err != nil {
			ctx.Log("warning", "     - [Job %s] could not clear reservation marker for %s: %v", job.ID, svc.Name, err)
		}
		// Optional model selection (#530): the backend's model-deploy contract
		// dispatches MODEL_CACHE_PULL (weights) then SERVICE_START
		// {service, model}. The model, when present, is persisted per-service
		// and injected into the engine's compose interpolation env.
		//
		// Optional VRAM budget (#577): a SERVICE_START that declares vram_mb /
		// vram_gb triggers node-side preemption of non-pinned services to make
		// room when the GPU lacks free VRAM. Absent => no preemption (the current
		// backend does not yet forward it; see parseRequiredVRAMBytes).
		//
		// Optional trust_remote_code (#848): a SERVICE_START that declares
		// trust_remote_code=true opts THIS deploy into vLLM's
		// --trust-remote-code (needed by models shipping custom code, e.g.
		// gte-multilingual-base, some Qwen/InternLM). Absent => leave any
		// persisted opt-in as-is; an explicit falsy value clears it (opt-in,
		// default OFF, non-sticky -- see parseTrustRemoteCodeIntent). Like
		// vram_mb, the aceteam backend does not yet forward this field, so it
		// is inert until it does; the aceteam-side follow-up is catalog
		// metadata marking which models need it so the deploy path can set it
		// automatically (and clear it for every other deploy to the same
		// service).
		//
		// Optional RAM budget (#831): mirrors vram_mb/vram_gb exactly --
		// parseRequiredRAMBytes reads ram_mb/ram_gb from the payload, which the
		// aceteam backend does not send today either. requiredVRAMBytes now
		// resolves through resolveRequiredVRAMBytes rather than
		// parseRequiredVRAMBytes directly: payload still wins when present, but
		// when CITADEL_RESOURCE_ISOLATION is opted in and the payload carries
		// nothing, it falls back to this node's own per-engine VRAM estimate
		// (status.EngineVRAMEstimateMB) so preemption works without waiting on
		// the backend (see resolveRequiredVRAMBytes).
		return h.serviceStart(ctx, svc, job.Payload["model"],
			resolveRequiredVRAMBytes(svc.Name, job.Payload), parseRequiredRAMBytes(job.Payload),
			parseTrustRemoteCodeIntent(job.Payload))
	case "SERVICE_STOP":
		// A remote SERVICE_STOP is operator/cloud intent: mark the service
		// durably stopped FIRST (mirrors liveModuleOps.Stop) so the stop
		// survives a worker restart / reboot even if the compose down below is
		// interrupted (#528). Deliberately NOT done in StopServiceByName: the
		// auto-stop-when-idle reconciler (#416) evicts actual state, not desired
		// state, and must not prevent an evicted service from starting on boot.
		if err := h.setDesiredStatusInManifestFile(svc.Name, "stopped"); err != nil {
			ctx.Log("warning", "     - [Job %s] could not set stopped marker for %s: %v", job.ID, svc.Name, err)
		}
		// Also clear any job-scoped reservation eviction tag (#832): an explicit
		// operator/platform stop is its own reason for the service being down,
		// so it must NOT later be restarted by an unrelated reservation's
		// Release just because it happens to still carry that reservation's
		// job id from an earlier eviction. Best-effort, same as above.
		if err := h.setEvictedMarkersInManifestFile(svc.Name, "", ""); err != nil {
			ctx.Log("warning", "     - [Job %s] could not clear reservation marker for %s: %v", job.ID, svc.Name, err)
		}
		return h.serviceStop(ctx, svc)
	default:
		return nil, fmt.Errorf("unknown service job type: %s", job.Type)
	}
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

func (h *ServiceHandler) serviceStatus(svc manifestService) ([]byte, error) {
	kind := h.resolveKind(svc)
	running := false

	switch kind {
	case "native":
		// Report what the engine can actually do, not whether a process name
		// matched (#649): a status of "running" is what the platform trusts when
		// it decides to keep routing inference here.
		running = services.IsNativeServiceServing(svc.Name)
	case "docker":
		running = h.isDockerServiceRunning(svc.Name)
	}

	return json.Marshal(serviceResult{
		Name:    svc.Name,
		Running: running,
		Kind:    kind,
		Action:  "status",
		Message: fmt.Sprintf("%s is %s (%s)", svc.Name, boolToStatus(running), kind),
	})
}

// serviceStart starts a manifest service. model is the optional model id a
// SERVICE_START job selected (#530): when non-empty and the engine supports a
// serve-time model (serviceModelEnvVar), it is persisted to the sibling
// <name>.env BEFORE the already-running short-circuit, so a model change on a
// running engine falls through to `up -d --force-recreate` and reloads it.
// trustRemoteCode is the optional --trust-remote-code intent (#848); same
// persist-then-recreate treatment as model, via persistServiceTrustRemoteCode.
// requiredRAMBytes is the optional RAM budget (#831) applyRAMIsolation's
// preflight enforces; like requiredVRAMBytes it is 0 when no budget is known,
// which always fits (fail-safe on an absent signal).
func (h *ServiceHandler) serviceStart(ctx JobContext, svc manifestService, model string, requiredVRAMBytes, requiredRAMBytes uint64, trustRemoteCode trustRemoteCodeIntent) ([]byte, error) {
	kind := h.resolveKind(svc)
	var err error
	// appliedModel is the model this start serves via compose env interpolation;
	// empty when no model was requested or the engine takes none.
	appliedModel := ""

	switch kind {
	case "native":
		// Native ollama has no compose env to inject, but its model contract is
		// pull-based: SERVICE_START {service: ollama, model: X} must ensure X is
		// pulled (idempotent, fast when cached) so the deploy contract holds even
		// when the preceding MODEL_CACHE_PULL failed or was missed (#543). Other
		// native engines have no pull mechanism; their model param stays ignored.
		pullModel := ""
		if model != "" {
			if svc.Name == "ollama" {
				pullModel = model
			} else {
				ctx.Log("info", "     - Service %s runs natively; model %q ignored (no pull mechanism for this engine)", svc.Name, model)
			}
		}
		// Serving, not merely process-present (#649). A SERVICE_START against a
		// dead-but-process-matching engine must actually start it; short-circuiting
		// here was how a deploy reported success onto a node serving nothing.
		alreadyRunning := services.IsNativeServiceServing(svc.Name)
		if !alreadyRunning {
			logDir := filepath.Join(h.ConfigDir, "logs")
			_, err = services.StartNativeService(svc.Name, logDir)
		}
		if err == nil && pullModel != "" {
			// Hard error on pull failure (job FAILURE), deliberately unlike the
			// soft serviceResult.Error path below: a deploy that could not make
			// the model available must not report success (#543).
			if pullErr := ensureOllamaModel(ctx, pullModel, !alreadyRunning); pullErr != nil {
				return nil, pullErr
			}
			appliedModel = pullModel
		}
		if alreadyRunning {
			msg := svc.Name + " is already running"
			if appliedModel != "" {
				msg = fmt.Sprintf("%s is already running; model %s pulled and available", svc.Name, appliedModel)
			}
			return json.Marshal(serviceResult{
				Name: svc.Name, Running: true, Kind: kind,
				Action: "start", Message: msg,
			})
		}

	case "docker":
		modelChanged := false
		if model != "" {
			envVar, changed, mErr := h.persistServiceModel(ctx, svc, model)
			if mErr != nil {
				return nil, fmt.Errorf("failed to persist model for %s: %w", svc.Name, mErr)
			}
			if envVar != "" {
				appliedModel = model
				modelChanged = changed
			}
		}
		// trustRemoteCode (#848): opt-in, default OFF, and explicitly
		// NON-sticky -- an absent field leaves a previously-persisted value
		// alone, but an explicit falsy value clears it (see
		// parseTrustRemoteCodeIntent's SECURITY note: without a disable path,
		// one deploy's opt-in would silently outlive the model that needed
		// it). Persisted BEFORE the already-running short-circuit, same as
		// model, so a change either direction on a running engine recreates
		// the container instead of silently leaving the old flag in force.
		trustChanged, tErr := h.persistServiceTrustRemoteCode(ctx, svc, trustRemoteCode)
		if tErr != nil {
			return nil, fmt.Errorf("failed to persist trust_remote_code for %s: %w", svc.Name, tErr)
		}
		// Already-running short-circuit, UNLESS the persisted model or
		// trust_remote_code flag just changed: then the running container serves
		// the old config and must be recreated.
		if !modelChanged && !trustChanged && h.isDockerServiceRunning(svc.Name) {
			msg := svc.Name + " is already running"
			if appliedModel != "" {
				msg = fmt.Sprintf("%s is already running serving %s", svc.Name, appliedModel)
			}
			return json.Marshal(serviceResult{
				Name: svc.Name, Running: true, Kind: kind,
				Action: "start", Message: msg,
			})
		}
		composePath, pathErr := h.resolveComposePath(svc)
		if pathErr != nil {
			return nil, pathErr
		}
		// ramOverridePath is the citadel#831 per-service RAM ceiling override
		// (empty when resource isolation is off, the target isn't GPU, or the
		// target is already running -- see applyRAMIsolation and the gate
		// below). Declared here so it is in scope for the composeArgs append
		// further down, alongside the sandbox override.
		var ramOverridePath string
		// Preempt non-pinned services to free VRAM for this deploy (#577), and
		// separately size/apply this deploy's own RAM ceiling (#831). Both are
		// gated on the target NOT already running: an already-running start
		// (including the model-change --force-recreate path below, which
		// reached here with modelChanged==true) already holds its own
		// VRAM/RAM, so evicting peers or resizing its ceiling would be a
		// needless disruption. Each returns an error — failing the deploy —
		// when its requirement cannot be met (VRAM: without evicting a pinned
		// service; RAM: a declared budget that doesn't fit — see
		// applyRAMIsolation's RAM-preflight doc comment for the fail-open/
		// fail-closed contract).
		if !h.isDockerServiceRunning(svc.Name) {
			if err := h.preemptForVRAM(ctx, svc, requiredVRAMBytes); err != nil {
				return nil, err
			}
			var ramErr error
			ramOverridePath, ramErr = h.applyRAMIsolation(ctx, svc, composePath, requiredRAMBytes)
			if ramErr != nil {
				return nil, ramErr
			}
		}
		// Transitional (#528): remove any container still under the legacy
		// "citadel-<name>" compose project (created by pre-fix TUI/config-apply
		// starts that passed `-p citadel-<name>`). The no-`-p` up below would
		// otherwise conflict on the pinned container_name -- a cross-project name
		// conflict that --force-recreate does NOT resolve.
		compose.RemoveLegacyProjectContainers("docker", svc.Name)
		// Include the least-privilege sandbox override when present (untrusted/
		// Tier-2 modules) so a remotely-started module also runs hardened -- the
		// override would otherwise be bypassed by this start site.
		composeArgs := []string{"compose", "-f", composePath}
		if override := catalog.ExistingSandboxOverride(filepath.Dir(composePath),
			strings.TrimSuffix(filepath.Base(composePath), filepath.Ext(filepath.Base(composePath)))); override != "" {
			composeArgs = append(composeArgs, "-f", override)
		}
		// citadel#831 RAM ceiling override (empty when not applicable — see the
		// ramOverridePath assignment above and applyRAMIsolation).
		if ramOverridePath != "" {
			composeArgs = append(composeArgs, "-f", ramOverridePath)
		}
		// Pass the sibling config env (<name>.env) explicitly: docker compose
		// only auto-loads a file literally named ".env", so without --env-file
		// the persisted model selection (#530) and any catalog install-time
		// config would be invisible to interpolation. Mirrors cmd/service.go
		// composeFileArgs, so a model set via job is also served after a plain
		// `citadel work` boot and vice versa.
		composeArgs = append(composeArgs, compose.EnvFileArgs(composePath)...)
		// --force-recreate so the compose port mapping is always applied to the
		// running container. Without it, `up` will ADOPT an existing container
		// with the same container_name (e.g. one left by a prior failed/portless
		// attempt) and leave it untouched, so the newly-declared host port never
		// gets published (the container comes up with NetworkSettings.Ports == {}).
		// Same treatment as llamacpp_inference.go's restart path. See citadel-cli#415.
		composeArgs = append(composeArgs, "up", "-d", "--force-recreate")
		// Preflight (citadel #767): skip the exec ONLY when the engine CLI is
		// missing (that exec would fail immediately anyway) and report a
		// friendly diagnosis instead of the raw
		// `exec: "docker": executable file not found in $PATH`-style error
		// (this handler always drives "docker" directly, see the
		// exec.Command("docker", ...) below). A daemon that failed to answer
		// the preflight's probe is a WARNING, not a refusal: it may just be
		// slow, and the compose-up call below already surfaces docker's own
		// error if it truly is unreachable -- see platform.PreflightDockerStart.
		if refuseErr, warning := platform.PreflightDockerStart("docker"); refuseErr != nil {
			err = fmt.Errorf("docker compose up failed: %s", refuseErr)
		} else {
			if warning != "" {
				ctx.Log("warn", "     - docker preflight: %s", warning)
			}
			cmd := exec.Command("docker", composeArgs...)
			cmd.Env = h.composeEnv()
			out, cmdErr := cmd.CombinedOutput()
			if cmdErr != nil {
				err = fmt.Errorf("docker compose up failed: %s", strings.TrimSpace(string(out)))
			}
		}
	}

	if err != nil {
		return json.Marshal(serviceResult{
			Name: svc.Name, Running: false, Kind: kind,
			Action: "start", Error: err.Error(),
			Message: fmt.Sprintf("failed to start %s: %s", svc.Name, err),
		})
	}

	msg := fmt.Sprintf("%s started successfully", svc.Name)
	if appliedModel != "" {
		msg = fmt.Sprintf("%s started successfully serving %s", svc.Name, appliedModel)
	}
	result := serviceResult{
		Name: svc.Name, Running: true, Kind: kind,
		Action: "start", Message: msg,
	}
	// For docker services, report the reachable host endpoint by inspecting the
	// container's published port bindings. This confirms the compose port
	// mapping was actually applied to the running container and tells the caller
	// where to reach the provisioned service. A missing binding surfaces the
	// #415 "no published ports" failure instead of silently reporting success.
	if kind == "docker" {
		if endpoint := h.dockerServiceEndpoint(svc.Name); endpoint != "" {
			result.Endpoint = endpoint
			result.Message = fmt.Sprintf("%s; reachable at %s", msg, endpoint)
		}
	}
	return json.Marshal(result)
}

func (h *ServiceHandler) serviceStop(ctx JobContext, svc manifestService) ([]byte, error) {
	kind := h.resolveKind(svc)
	var err error

	switch kind {
	case "native":
		// Deliberately the PROCESS check, not IsNativeServiceServing (#649): a
		// wedged engine that has stopped answering is still a live process
		// holding VRAM, and stop must kill it. Using the serving probe here would
		// report "not running" and leave it alive forever -- the one place where
		// the loose predicate is the correct one.
		if !services.IsNativeServiceRunning(svc.Name) {
			return json.Marshal(serviceResult{
				Name: svc.Name, Running: false, Kind: kind,
				Action: "stop", Message: svc.Name + " is not running",
			})
		}
		err = services.StopNativeService(svc.Name)

	case "docker":
		if !h.isDockerServiceRunning(svc.Name) {
			return json.Marshal(serviceResult{
				Name: svc.Name, Running: false, Kind: kind,
				Action: "stop", Message: svc.Name + " is not running",
			})
		}
		composePath, pathErr := h.resolveComposePath(svc)
		if pathErr != nil {
			return nil, pathErr
		}
		// The sibling env is passed on down too (mirrors composeFileArgs) so a
		// compose file whose interpolation hard-requires a config var still
		// resolves; a no-op when no <name>.env exists.
		downArgs := []string{"compose", "-f", composePath}
		downArgs = append(downArgs, compose.EnvFileArgs(composePath)...)
		downArgs = append(downArgs, "down")
		cmd := exec.Command("docker", downArgs...)
		cmd.Env = h.composeEnv()
		out, cmdErr := cmd.CombinedOutput()
		if cmdErr != nil {
			err = fmt.Errorf("docker compose down failed: %s", strings.TrimSpace(string(out)))
		}
		// Transitional (#528): also remove containers a pre-fix start left under
		// the legacy "citadel-<name>" compose project, which the no-`-p` down
		// above cannot see (that mismatch was the silent stop no-op of #528).
		compose.RemoveLegacyProjectContainers("docker", svc.Name)
	}

	if err != nil {
		return json.Marshal(serviceResult{
			Name: svc.Name, Running: false, Kind: kind,
			Action: "stop", Error: err.Error(),
			Message: fmt.Sprintf("failed to stop %s: %s", svc.Name, err),
		})
	}

	return json.Marshal(serviceResult{
		Name: svc.Name, Running: false, Kind: kind,
		Action: "stop", Message: fmt.Sprintf("%s stopped successfully", svc.Name),
	})
}

// StopServiceByName stops a manifest-declared or embedded managed service by
// its logical name, without a remote job. It is the programmatic entry point
// used by the config-gated auto-stop-when-idle reconciler (citadel #416): the
// reconciler decides WHAT to evict; this reuses the same compose "down" path a
// SERVICE_STOP job would take so there is one stop implementation. A service
// absent from the manifest and not embedded is reported as an error (the
// reconciler logs and moves on). A service that is already stopped is a no-op.
func (h *ServiceHandler) StopServiceByName(name string) error {
	manifest, err := h.loadManifest()
	if err != nil {
		return fmt.Errorf("failed to load manifest: %w", err)
	}
	svc, ok := h.findService(manifest, name)
	if !ok {
		if _, embedded := embeddedservices.ServiceMap[name]; !embedded {
			return fmt.Errorf("service %q not found in manifest", name)
		}
		svc, err = h.materializeEmbeddedService(name)
		if err != nil {
			return fmt.Errorf("failed to reconcile embedded service %q: %w", name, err)
		}
	}
	// Silent JobContext: there is no remote job to report progress against.
	res, err := h.serviceStop(JobContext{LogFn: func(string, string) {}}, svc)
	if err != nil {
		return err
	}
	var parsed serviceResult
	if json.Unmarshal(res, &parsed) == nil && parsed.Error != "" {
		return fmt.Errorf("%s", parsed.Error)
	}
	return nil
}

// StartServiceByName starts a manifest-declared or embedded managed service by
// its logical name, without a remote job — the start-side counterpart of
// StopServiceByName. It is the programmatic entry point used by the
// reservation primitive's Release (#832, reservation.go): a reservation
// decides WHICH services to restart; this reuses the same start implementation
// a SERVICE_START job would take, with no model change and no VRAM budget (so
// restoring a reservation never itself recurses into another eviction). A
// service absent from the manifest and not embedded is reported as an error.
// Like StopServiceByName, this does NOT touch desired_status or the
// reservation markers — callers own that.
func (h *ServiceHandler) StartServiceByName(name string) error {
	manifest, err := h.loadManifest()
	if err != nil {
		return fmt.Errorf("failed to load manifest: %w", err)
	}
	svc, ok := h.findService(manifest, name)
	if !ok {
		if _, embedded := embeddedservices.ServiceMap[name]; !embedded {
			return fmt.Errorf("service %q not found in manifest", name)
		}
		svc, err = h.materializeEmbeddedService(name)
		if err != nil {
			return fmt.Errorf("failed to reconcile embedded service %q: %w", name, err)
		}
	}
	// Silent JobContext: there is no remote job to report progress against.
	// trustRemoteCodeUnspecified leaves any already-persisted trust setting
	// untouched (see parseTrustRemoteCodeIntent) -- restoring a reservation
	// must not itself change trust posture.
	res, err := h.serviceStart(JobContext{LogFn: func(string, string) {}}, svc, "", 0, 0, trustRemoteCodeUnspecified)
	if err != nil {
		return err
	}
	var parsed serviceResult
	if json.Unmarshal(res, &parsed) == nil && parsed.Error != "" {
		return fmt.Errorf("%s", parsed.Error)
	}
	return nil
}

// ---------------------------------------------------------------------------
// VRAM preemption + node pinning (#577)
// ---------------------------------------------------------------------------

// parseRequiredVRAMBytes reads the optional VRAM budget a SERVICE_START job
// declares, in bytes. It accepts vram_mb (preferred) or vram_gb; a missing,
// blank, non-numeric, or non-positive value yields 0 (= "no requirement", which
// disables preemption). NOTE: the current aceteam backend does NOT yet forward a
// VRAM field on SERVICE_START (fabric_provision dispatches only {service, model};
// the DeployModel API's vram_budget_gb is explicitly "not yet forwarded to
// Citadel"). So preemption is INERT until the backend sends one of these keys —
// wiring that up (forward #6018's VRAM-fit budget as vram_mb) is the aceteam-side
// follow-up.
func parseRequiredVRAMBytes(payload map[string]string) uint64 {
	if v := strings.TrimSpace(payload["vram_mb"]); v != "" {
		if mb, err := strconv.ParseFloat(v, 64); err == nil && mb > 0 {
			return uint64(mb * 1024 * 1024)
		}
	}
	if v := strings.TrimSpace(payload["vram_gb"]); v != "" {
		if gb, err := strconv.ParseFloat(v, 64); err == nil && gb > 0 {
			return uint64(gb * 1024 * 1024 * 1024)
		}
	}
	return 0
}

// resolveRequiredVRAMBytes decides the VRAM budget (bytes) preemptForVRAM
// enforces for a SERVICE_START (citadel#831, wiring #577's preflight to a
// citadel-side estimate so it works without waiting on the backend):
//
//  1. The payload-declared value always wins when present
//     (parseRequiredVRAMBytes > 0) — an explicit backend-provided budget is
//     always more precise than anything this node can guess.
//  2. Otherwise, ONLY when CITADEL_RESOURCE_ISOLATION is opted in
//     (resourceIsolationEnabled), fall back to this node's own per-engine
//     VRAM estimate: status.EngineVRAMEstimateMB, the SAME provisioning-budget
//     table the model-hotswap swap planner already uses as ITS fallback
//     (citadel#689) when it has never measured the (engine, model) pair being
//     swapped in. Reusing it here means no new numbers to invent or keep in
//     sync — one table, two consumers.
//  3. Absent both, returns 0 — today's inert behavior (#577 shipped gated on
//     a payload field the backend never sends), UNCHANGED while the flag is
//     off. This is the deliberate safe-by-default posture: flipping on VRAM
//     preemption for every deploy is a real behavior change (it can now durably
//     stop other services on a node that has never had that happen before), so
//     it stays off until an operator opts in on a node they've reviewed.
func resolveRequiredVRAMBytes(svcName string, payload map[string]string) uint64 {
	if v := parseRequiredVRAMBytes(payload); v > 0 {
		return v
	}
	if !resourceIsolationEnabled() {
		return 0
	}
	mb := status.EngineVRAMEstimateMB(svcName)
	if mb <= 0 {
		return 0
	}
	return uint64(mb) * 1024 * 1024
}

// ---------------------------------------------------------------------------
// RAM isolation (citadel#831): per-service mem_limit ceiling + RAM preflight
// ---------------------------------------------------------------------------
//
// See docs/design-resource-isolation.md §2/§4 and the owner's design-decision
// comment on citadel#831 (2026-08-25): RAM gets a REAL cgroup limit (unlike
// VRAM, which has no hardware/driver mechanism to hard-cap on the 3090 target
// fleet — preflight/preemption only). Both this and resolveRequiredVRAMBytes's
// citadel-side VRAM estimate above are gated behind the SAME opt-in flag: they
// are the two halves of #831 v1, both newly able to refuse or durably stop a
// deploy on a node that has never seen that happen, so both stay off by
// default until an operator opts in — matching this codebase's convention for
// every other toggle that can change eviction/refusal behavior
// (CITADEL_GROUNDING_GUARDRAIL, SERVICE_AUTO_STOP_WHEN_IDLE,
// CITADEL_ENERGY_SAMPLING, ...).

// resourceIsolationEnabled reports whether citadel#831's v1 mechanisms are
// active on this node. Default OFF; truthy 1/true/yes/on
// (case/whitespace-insensitive), matching every other opt-in toggle in this
// codebase. A garbage/unset value stays OFF (unlike CITADEL_MODEL_HOTSWAP's
// break-glass-disable convention, which defaults ON — this is the opposite
// direction: a NEW capability that can newly refuse/evict, not an existing
// one being turned off).
func resourceIsolationEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CITADEL_RESOURCE_ISOLATION"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseRequiredRAMBytes reads the optional RAM budget a SERVICE_START job
// declares, in bytes. Deliberately mirrors parseRequiredVRAMBytes's shape and
// naming exactly (ram_mb preferred, else ram_gb; a missing/blank/non-numeric/
// non-positive value yields 0 = "no requirement declared"). The aceteam
// backend does not send either key today — same pre-#831 state vram_mb/
// vram_gb were in before #577 — so this exists mainly to make the preflight
// forward-compatible with a future backend-declared budget; PlanRAMPreflight
// fails OPEN (never refuses) when this returns 0, matching #577/#828's
// identical fail-safe-on-absent-signal contract.
func parseRequiredRAMBytes(payload map[string]string) uint64 {
	if v := strings.TrimSpace(payload["ram_mb"]); v != "" {
		if mb, err := strconv.ParseFloat(v, 64); err == nil && mb > 0 {
			return uint64(mb * 1024 * 1024)
		}
	}
	if v := strings.TrimSpace(payload["ram_gb"]); v != "" {
		if gb, err := strconv.ParseFloat(v, 64); err == nil && gb > 0 {
			return uint64(gb * 1024 * 1024 * 1024)
		}
	}
	return 0
}

// pinnedRAMBytes sums the RAM footprint currently attributed to RUNNING
// pinned services, excluding the deploy target itself. Mirrors
// buildPreemptCandidates' enumeration (RUNNING managed services only), but
// only needs the RAM sum — RAM isolation does not preempt (that's a
// documented follow-up per the design doc; RAM safety here comes entirely
// from the per-service cgroup ceiling, not eviction).
func pinnedRAMBytes(st *status.NodeStatus, exclude string, pinned map[string]bool) uint64 {
	var total uint64
	for i := range st.Services {
		s := &st.Services[i]
		if s.Name == exclude || s.Status != status.ServiceStatusRunning || !pinned[s.Name] {
			continue
		}
		if s.Footprint != nil {
			total += s.Footprint.RAMBytes
		}
	}
	return total
}

// applyRAMIsolation computes and writes the per-service RAM ceiling override
// for a GPU service about to start (citadel#831 §2), and separately refuses
// the start (RAM preflight, §4) when a declared RAM requirement does not fit.
// Returns the override file path to append as an additional compose `-f`
// (empty when isolation is off, the target isn't a GPU service, or nothing
// else applies). A non-nil error is a hard refusal: the caller MUST fail the
// deploy (job FAILURE), per the owner's citadel#831 design decision ("refuse
// fast, clear error").
//
// Fails OPEN (skips isolation silently, does NOT refuse) on any
// signal-collection error — unreadable compose, uncollectable node status,
// unreadable manifest — mirroring #828's disk-preflight policy exactly: an
// estimation/signal failure must never turn a previously-working deploy into
// a new failure mode. The ONLY refusing case is a CONFIRMED shortfall: a
// declared RAM requirement that a successfully-collected budget says will not
// fit (PlanRAMPreflight).
func (h *ServiceHandler) applyRAMIsolation(ctx JobContext, svc manifestService, composePath string, requiredRAMBytes uint64) (string, error) {
	if !resourceIsolationEnabled() {
		return "", nil
	}
	baseContent, readErr := os.ReadFile(composePath)
	if readErr != nil {
		ctx.Log("warning", "     - [ram] could not read compose for %s: %v; skipping RAM isolation", svc.Name, readErr)
		return "", nil
	}
	isGPU, gpuErr := catalog.ComposeDeclaresGPU(string(baseContent))
	if gpuErr != nil {
		ctx.Log("warning", "     - [ram] could not parse compose for %s: %v; skipping RAM isolation", svc.Name, gpuErr)
		return "", nil
	}
	if !isGPU {
		return "", nil // RAM isolation is scoped to GPU/media services (§2); non-GPU services are unaffected
	}

	st, err := h.collectNodeStatus()
	if err != nil {
		ctx.Log("warning", "     - [ram] could not collect node status: %v; skipping RAM isolation for %s", err, svc.Name)
		return "", nil
	}
	// Reuses freeVRAMBytes's own "found" signal for hostHasGPU: it is already
	// the exact fail-safe GPU-presence check this file uses for VRAM
	// (freeVRAMBytes, #833), so this stays consistent with
	// GenerateGPUMemoryOverride's own hostHasGPU gate without an extra
	// nvidia-smi probe.
	_, hostHasGPU := freeVRAMBytes(st.GPU)
	if !hostHasGPU {
		return "", nil
	}

	manifest, mErr := h.loadManifest()
	if mErr != nil {
		ctx.Log("warning", "     - [ram] could not load manifest: %v; skipping RAM isolation for %s", mErr, svc.Name)
		return "", nil
	}
	pinned := manifest.pinnedSet()
	pinnedRAM := pinnedRAMBytes(st, svc.Name, pinned)
	var availableRAM uint64
	if st.System.MemoryAvailableGB > 0 {
		availableRAM = uint64(st.System.MemoryAvailableGB * float64(1<<30))
	}

	// RAM preflight (§4): refuse ONLY on a confirmed shortfall against a
	// declared requirement; requiredRAMBytes==0 (no payload field) always Fits.
	plan := status.PlanRAMPreflight(requiredRAMBytes, availableRAM, pinnedRAM)
	if !plan.Fits {
		return "", fmt.Errorf("cannot start %s: %s", svc.Name, plan.Reason)
	}

	// RAMBudgetBytes returns 0 when no safe ceiling can be derived (a
	// transiently tight reading, e.g.) -- GenerateGPUMemoryOverride rejects a
	// non-positive limit, which the genErr branch below treats as fail-open
	// (skip isolation this start) rather than applying a fabricated small
	// cap. See RAMBudgetBytes' doc comment for why returning 0 here, not a
	// clamped floor, is the safe direction.
	ceiling := status.RAMBudgetBytes(availableRAM, pinnedRAM)
	override, genErr := catalog.GenerateGPUMemoryOverride(string(baseContent), int64(ceiling), hostHasGPU)
	if genErr != nil {
		ctx.Log("warning", "     - [ram] could not derive a safe RAM ceiling for %s (%v); skipping RAM isolation for this start", svc.Name, genErr)
		return "", nil
	}
	if override == "" {
		return "", nil // nothing to override (e.g. base compose already sets its own mem_limit)
	}

	dir := filepath.Dir(composePath)
	name := strings.TrimSuffix(filepath.Base(composePath), filepath.Ext(filepath.Base(composePath)))
	overridePath := catalog.GPURAMOverridePath(dir, name)
	// 0600, matching every other manifest/override write in this tree
	// (writeManifestBytes, the sandbox override) -- not 0644.
	if writeErr := os.WriteFile(overridePath, []byte(override), 0600); writeErr != nil {
		ctx.Log("warning", "     - [ram] could not write RAM override for %s: %v; skipping RAM isolation", svc.Name, writeErr)
		return "", nil
	}
	ctx.Log("info", "     - [ram] applying mem_limit ceiling of %.1fGB to %s (%.1fGB available, %.1fGB reserved for pinned services + headroom)",
		float64(ceiling)/(1<<30), svc.Name, float64(availableRAM)/(1<<30), float64(pinnedRAM)/(1<<30))
	return overridePath, nil
}

// ---------------------------------------------------------------------------
// --trust-remote-code opt-in (citadel#848)
// ---------------------------------------------------------------------------

// trustRemoteCodeIntent is the tri-state result of parsing the optional
// SERVICE_START trust_remote_code payload field: a SERVICE_START that omits
// the field (the common/current case -- see parseTrustRemoteCodeIntent) must
// leave a previously-persisted opt-in untouched, which a plain bool cannot
// distinguish from "explicitly turn it off".
type trustRemoteCodeIntent int

const (
	// trustRemoteCodeUnspecified: the payload did not address trust_remote_code
	// at all. Leaves any persisted value as-is (e.g. a plain restart, or a
	// deploy of a DIFFERENT model that doesn't mention the field).
	trustRemoteCodeUnspecified trustRemoteCodeIntent = iota
	// trustRemoteCodeEnable: persist the opt-in.
	trustRemoteCodeEnable
	// trustRemoteCodeDisable: explicitly clear a previously-persisted opt-in.
	// SECURITY (this is the load-bearing case, not a nicety): without it, once
	// one deploy opts a service into --trust-remote-code, EVERY later deploy of
	// a DIFFERENT model to that same service would keep running with it too --
	// arbitrary code execution silently outliving the one model that needed it.
	trustRemoteCodeDisable
)

// parseTrustRemoteCodeIntent reads the optional SERVICE_START payload flag
// that opts a deploy into vLLM's --trust-remote-code -- required by models
// that ship custom modeling code (e.g. gte-multilingual-base, some Qwen/
// InternLM), which otherwise crash Exited(1) at model-config creation.
// SECURITY: --trust-remote-code executes arbitrary Python from the model
// repo, so this must default OFF and only ever be turned on by explicit
// signal here -- never baked into the compose template unconditionally (see
// services/compose/vllm.yml). A missing/blank value is trustRemoteCodeUnspecified
// (leave persisted state alone); a truthy value ("1"/"true"/"yes"/"on",
// mirroring the convention used throughout this codebase -- energy sampling,
// self-heal, etc. -- case/whitespace-insensitive) is Enable; any other
// non-blank value (e.g. "false"/"0"/"no") is Disable, so a later deploy can
// explicitly turn a previous opt-in back off.
//
// NOTE: like vram_mb/vram_gb (#577), the aceteam backend does NOT yet forward
// this field on SERVICE_START (fabric_provision dispatches only
// {service, model}), so this is INERT until the backend sends it. The
// aceteam-side follow-up is catalog metadata marking which models require
// trust_remote_code, so the deploy path can set it automatically ONLY for
// those models -- and MUST send an explicit disable for every other deploy to
// the same service, or this tri-state design buys nothing -- tracked
// separately, not built here.
func parseTrustRemoteCodeIntent(payload map[string]string) trustRemoteCodeIntent {
	v, ok := payload["trust_remote_code"]
	if !ok {
		return trustRemoteCodeUnspecified
	}
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return trustRemoteCodeUnspecified
	}
	switch strings.ToLower(trimmed) {
	case "1", "true", "yes", "on":
		return trustRemoteCodeEnable
	}
	return trustRemoteCodeDisable
}

// preemptForVRAM makes room for a deploy that declares a VRAM budget by durably
// stopping non-pinned services until it fits. It is a no-op when the requirement
// is unknown (requiredVRAMBytes==0), when free VRAM already suffices, or when the
// node's free VRAM cannot be determined (fail-safe: never evict on an absent
// signal). It returns an error — FAILING the deploy — when the requirement cannot
// be met without evicting a pinned service (pinned_services allowlist, #577).
//
// Durability: each preempted service is stopped via the SERVICE_STOP path
// (desired_status: stopped, then compose down), NOT a bare docker stop, so the
// boot/manifest-reconcile paths do not restart it out from under the incoming
// deploy — the VRAM-cascade gotcha from #528. This means preemption is STICKY: an
// evicted service stays down across reboots until an explicit SERVICE_START (which
// clears the marker) brings it back.
func (h *ServiceHandler) preemptForVRAM(ctx JobContext, svc manifestService, requiredVRAMBytes uint64) error {
	if requiredVRAMBytes == 0 {
		return nil // no declared budget => nothing to enforce
	}

	// Collect live node status once: free VRAM + per-service VRAM footprints. A
	// fresh collector's debounced IdleState is unreliable here (no history), so
	// the idle ORDERING signal is derived instantaneously from the footprint.
	st, err := h.collectNodeStatus()
	if err != nil {
		ctx.Log("warning", "     - [preempt] could not collect node status: %v; skipping VRAM fit check", err)
		return nil
	}
	freeVRAM, ok := freeVRAMBytes(st.GPU)
	if !ok {
		ctx.Log("info", "     - [preempt] GPU free VRAM unknown; skipping VRAM fit check for %s", svc.Name)
		return nil
	}

	manifest, err := h.loadManifest()
	if err != nil {
		return fmt.Errorf("failed to load manifest for preemption: %w", err)
	}
	pinned := manifest.pinnedSet()

	candidates := buildPreemptCandidates(st, svc.Name, pinned)
	plan := status.PlanPreemption(candidates, requiredVRAMBytes, freeVRAM)
	if !plan.Fits {
		// Cannot fit without evicting a pinned service: reject the deploy.
		return fmt.Errorf("cannot start %s: %s", svc.Name, plan.Reason)
	}
	if len(plan.Stop) == 0 {
		return nil // already fits; no eviction needed
	}

	ctx.Log("info", "     - [preempt] %s", plan.Reason)
	for _, name := range plan.Stop {
		ctx.Log("info", "     - [preempt] durably stopping %s to free VRAM for %s", name, svc.Name)
		// Durable FIRST (mirrors the SERVICE_STOP job path): mark stopped so the
		// manifest reconcile does not restart it, then compose down.
		if err := h.setDesiredStatusInManifestFile(name, "stopped"); err != nil {
			ctx.Log("warning", "     - [preempt] could not mark %s stopped: %v", name, err)
		}
		if err := h.stopByName(name); err != nil {
			// A failed eviction leaves VRAM unfreed: fail the deploy rather than
			// start into insufficient VRAM.
			return fmt.Errorf("cannot start %s: failed to preempt %s: %w", svc.Name, name, err)
		}
	}
	return nil
}

// collectNodeStatus returns live node status, via the injected collectStatus
// override when set (tests), else a real status.NewCollector collection. See
// the ServiceHandler.collectStatus field doc.
func (h *ServiceHandler) collectNodeStatus() (*status.NodeStatus, error) {
	if h.collectStatus != nil {
		return h.collectStatus()
	}
	collector := status.NewCollector(status.CollectorConfig{ConfigDir: h.ConfigDir})
	return collector.Collect()
}

// stopByName routes through the injected stopServiceFn override when set
// (tests), else the real StopServiceByName. See the ServiceHandler
// stopServiceFn field doc.
func (h *ServiceHandler) stopByName(name string) error {
	if h.stopServiceFn != nil {
		return h.stopServiceFn(name)
	}
	return h.StopServiceByName(name)
}

// startByName routes through the injected startServiceFn override when set
// (tests), else the real StartServiceByName. See the ServiceHandler
// startServiceFn field doc.
func (h *ServiceHandler) startByName(name string) error {
	if h.startServiceFn != nil {
		return h.startServiceFn(name)
	}
	return h.StartServiceByName(name)
}

// writeManifestBytes performs the final write in every yaml.Node-surgery
// manifest setter, via the injected writeManifestFn override when set
// (tests), else a real os.WriteFile. See the ServiceHandler.writeManifestFn
// field doc.
func (h *ServiceHandler) writeManifestBytes(path string, data []byte) error {
	if h.writeManifestFn != nil {
		return h.writeManifestFn(path, data)
	}
	return os.WriteFile(path, data, 0600)
}

// freeVRAMBytes sums the currently-free VRAM across all GPUs that report a
// memory total, in bytes. The bool is false when NO GPU reports a memory total
// (no GPU / nvidia-smi absent), so callers skip the VRAM fit check rather than
// treat "unknown" as "zero free".
//
// Prefers MemoryFreeMB (citadel #833) — nvidia-smi's own memory.free — over
// the derived MemoryTotalMB-MemoryUsedMB whenever it's populated. The two are
// NOT equivalent: nvidia-smi reserves some memory (driver/ECC overhead) that
// counts against neither total-as-free nor used, so the derived value
// systematically overstates what's actually free (measured drift on a real
// RTX 3090: 457MiB) — the wrong direction of error for a value gating whether
// a deploy fits. Falls back to the derived value only when MemoryFreeMB is
// unset (older/partial GPU reporting, e.g. macOS/Metal).
func freeVRAMBytes(gpus []status.GPUMetrics) (uint64, bool) {
	var free uint64
	found := false
	for _, g := range gpus {
		if g.MemoryTotalMB <= 0 {
			continue
		}
		found = true
		f := g.MemoryFreeMB
		if f <= 0 {
			f = g.MemoryTotalMB - g.MemoryUsedMB
		}
		if f < 0 {
			f = 0
		}
		free += uint64(f) * 1024 * 1024
	}
	return free, found
}

// buildPreemptCandidates turns the live node status into the pure inputs for
// status.PlanPreemption: every RUNNING managed service except the deploy target,
// tagged with its VRAM footprint, an instantaneous idle signal (for ordering),
// and whether it is pinned. Catalog apps are intentionally excluded — pinning is
// a service-level allowlist and the eviction path here is the service compose
// down.
func buildPreemptCandidates(st *status.NodeStatus, exclude string, pinned map[string]bool) []status.PreemptCandidate {
	out := make([]status.PreemptCandidate, 0, len(st.Services))
	for i := range st.Services {
		s := &st.Services[i]
		if s.Name == exclude || s.Status != status.ServiceStatusRunning {
			continue
		}
		var vram uint64
		if s.Footprint != nil {
			vram = s.Footprint.VRAMBytes
		}
		out = append(out, status.PreemptCandidate{
			Name:      s.Name,
			VRAMBytes: vram,
			// Instantaneous (not debounced) idle: a fresh collector has no idle
			// history, so use the footprint's CPU/GPU activity directly.
			Idle:   !status.FootprintActive(s.Footprint),
			Pinned: pinned[s.Name],
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Model selection (#530)
// ---------------------------------------------------------------------------

// serviceModelEnvVar maps a managed engine to the compose interpolation
// variable that selects its served model (#530). Engines absent from this map
// take no serve-time model parameter: ollama loads models on demand at request
// time (nothing to configure at serve time), and llamacpp/sglang are driven by
// a whole command line (LLAMACPP_COMMAND / SGLANG_COMMAND) rather than a bare
// model id — wiring a model into those is deliberately out of scope here.
var serviceModelEnvVar = map[string]string{
	"vllm": "VLLM_MODEL",
}

// modelIDPattern is the conservative allowlist for model identifiers persisted
// to the sibling env file: broad enough for HuggingFace ids (org/name with
// dots, dashes, underscores, optional :revision) while rejecting whitespace,
// quotes, '#', '$' and control characters that could corrupt the env file or
// leak into compose interpolation. The model is backend-controlled input, but
// it is written to a file compose parses — validate anyway.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

// persistServiceModel records the model a SERVICE_START job selected for a
// service, writing <envVar>=<model> into the sibling <name>.env next to the
// service's compose file. That file is passed to compose via --env-file on
// BOTH start paths — this handler and the cmd/ boot path (composeFileArgs) —
// and both derive the compose path the same way (ConfigDir + manifest
// compose_file via ValidatePathWithinDir), so a model set via job is still
// served after a plain `citadel work` boot. Returns the env var used (empty
// when the engine has no serve-time model parameter — logged, not an error)
// and whether the persisted value actually changed (callers skip the engine
// reload when it did not, so a re-dispatched identical SERVICE_START does not
// thrash a running engine with a multi-minute model reload).
func (h *ServiceHandler) persistServiceModel(ctx JobContext, svc manifestService, model string) (string, bool, error) {
	envVar, ok := serviceModelEnvVar[svc.Name]
	if !ok {
		ctx.Log("info", "     - Service %s has no serve-time model parameter; model %q not applied (ollama loads on demand; llamacpp/sglang use command-line env)", svc.Name, model)
		return "", false, nil
	}
	if !modelIDPattern.MatchString(model) {
		return "", false, fmt.Errorf("invalid model identifier %q", model)
	}
	composePath, err := h.resolveComposePath(svc)
	if err != nil {
		return "", false, err
	}
	envPath := compose.SiblingEnvPath(composePath)
	if current, present := compose.ReadEnvVar(envPath, envVar); present && current == model {
		return envVar, false, nil
	}
	if err := compose.UpsertEnvVar(envPath, envVar, model); err != nil {
		return "", false, err
	}
	ctx.Log("info", "     - Persisted %s=%s to %s", envVar, model, envPath)
	return envVar, true, nil
}

// serviceTrustRemoteCodeEnvVar maps a managed engine to the compose
// interpolation variable that opts it into --trust-remote-code (#848).
// Deliberately its own map (not folded into serviceModelEnvVar): engines
// absent here take no trust-remote-code parameter at all, and keeping this
// list separate from the model-env map means adding a future engine to one
// never silently wires it into the other. vllm only, for now -- llamacpp/
// bonsai/ollama have no equivalent flag wired in their compose files.
var serviceTrustRemoteCodeEnvVar = map[string]string{
	"vllm": "VLLM_TRUST_REMOTE_CODE",
}

// persistServiceTrustRemoteCode applies a SERVICE_START job's trust_remote_code
// intent (#848) to the sibling <name>.env next to the service's compose file --
// the same file persistServiceModel writes to and both start paths (this
// handler and the cmd/ boot path) pass to compose via --env-file, so the
// setting survives a plain `citadel work` restart.
//
// intent==Unspecified is a no-op (never called by serviceStart, but safe if it
// is). intent==Enable writes <envVar>=1. intent==Disable writes <envVar>=
// (empty) rather than deleting the line: compose's ${VAR:+word} interpolation
// (services/compose/vllm.yml) treats a variable that is SET-BUT-EMPTY
// identically to UNSET -- both are "null" in POSIX parameter-expansion terms,
// which is exactly why the compose template uses the colon form (:+) and not
// the bare (+) form -- so an empty value reliably turns --trust-remote-code
// back off. This is the mechanism that makes the flag non-sticky: a later
// deploy of a DIFFERENT model to the same service can send an explicit
// "false" to clear an earlier opt-in (see parseTrustRemoteCodeIntent's
// SECURITY note -- without this, the opt-in would silently outlive the model
// that needed it). Returns whether the persisted value actually changed, so a
// re-dispatched identical SERVICE_START does not force an unnecessary
// container recreate.
func (h *ServiceHandler) persistServiceTrustRemoteCode(ctx JobContext, svc manifestService, intent trustRemoteCodeIntent) (bool, error) {
	if intent == trustRemoteCodeUnspecified {
		return false, nil
	}
	envVar, ok := serviceTrustRemoteCodeEnvVar[svc.Name]
	if !ok {
		ctx.Log("info", "     - Service %s has no --trust-remote-code parameter; trust_remote_code not applied", svc.Name)
		return false, nil
	}
	composePath, err := h.resolveComposePath(svc)
	if err != nil {
		return false, err
	}
	envPath := compose.SiblingEnvPath(composePath)
	current, present := compose.ReadEnvVar(envPath, envVar)

	switch intent {
	case trustRemoteCodeEnable:
		if present && current == "1" {
			return false, nil
		}
		if err := compose.UpsertEnvVar(envPath, envVar, "1"); err != nil {
			return false, err
		}
		ctx.Log("info", "     - Persisted %s=1 to %s (opt-in, #848)", envVar, envPath)
		return true, nil
	case trustRemoteCodeDisable:
		if !present || current == "" {
			return false, nil // already off / never set -- nothing to clear
		}
		if err := compose.UpsertEnvVar(envPath, envVar, ""); err != nil {
			return false, err
		}
		ctx.Log("info", "     - Cleared %s in %s (explicit opt-out, #848)", envVar, envPath)
		return true, nil
	}
	return false, nil
}

// ollamaServerWaitTimeout bounds the readiness poll before pulling on a
// freshly-started native ollama: `ollama pull` needs the server up.
const ollamaServerWaitTimeout = 30 * time.Second

// ensureOllamaModel makes a model available on a natively-running ollama by
// running `ollama pull <model>` (#543). The pull is idempotent and fast when
// the model is already cached, so it is safe to run on every SERVICE_START —
// it is the node-side guarantee that the deploy contract (MODEL_CACHE_PULL
// then SERVICE_START {service, model}) holds even when the weights pull was
// missed or dispatched to the wrong engine. Failures are returned as errors so
// the job reports FAILURE instead of silently serving nothing. When
// waitForServer is set (service just started), the server is polled via
// `ollama list` before pulling; on poll timeout the pull still runs and
// surfaces the real connection error.
func ensureOllamaModel(ctx JobContext, model string, waitForServer bool) error {
	// The model is backend-controlled input passed to exec: validate with the
	// same allowlist the compose env persistence uses.
	if !modelIDPattern.MatchString(model) {
		return fmt.Errorf("invalid model identifier %q", model)
	}
	if _, err := exec.LookPath("ollama"); err != nil {
		return fmt.Errorf("cannot pull model %q: ollama binary not found in PATH", model)
	}
	if waitForServer {
		deadline := time.Now().Add(ollamaServerWaitTimeout)
		for time.Now().Before(deadline) {
			if exec.Command("ollama", "list").Run() == nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
	ctx.Log("info", "     - Ensuring ollama model %q is pulled (idempotent; fast when cached)", model)
	out, err := runOllamaPull(model)
	if err != nil {
		return fmt.Errorf("ollama pull %s failed: %w: %s", model, err, strings.TrimSpace(string(out)))
	}
	ctx.Log("info", "     - Model %q pulled and available on ollama", model)

	// Cache index write (citadel #682 P2a, design doc §8.3): this is the
	// ONE pull site that does NOT run through ModelCachePullHandler
	// (MODEL_CACHE_PULL) -- it is the SERVICE_START native-ollama path
	// (#543). Missing it would reopen the #739 "every install path must
	// record itself, or it's invisible" gap under a new name. Best-effort,
	// like every other write site: a failed index update never fails the
	// SERVICE_START that already succeeded.
	upsertCacheIndexEntry(ctx, "", cacheindex.Entry{
		CacheDir:  embeddedservices.EngineCacheDirs["ollama"].Dir,
		Family:    embeddedservices.CacheFamilyNative,
		Model:     model,
		Engine:    "ollama",
		SizeBytes: ollamaModelSize(model),
	})
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (h *ServiceHandler) loadManifest() (*serviceManifest, error) {
	path := filepath.Join(h.ConfigDir, "citadel.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m serviceManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// materializeEmbeddedService makes a newly-embedded service (present in the
// binary's ServiceMap but absent from citadel.yaml) startable on this node. It
// writes the embedded compose file into ConfigDir/services/<name>.yml (if not
// already present) and additively registers a service block in citadel.yaml.
// It returns the resulting manifestService so the caller can proceed with the
// requested operation. The persist is additive and idempotent: it never removes
// or overwrites existing services, and preserves the rest of the manifest
// (node:, capabilities:, comments) untouched.
func (h *ServiceHandler) materializeEmbeddedService(name string) (manifestService, error) {
	svc := manifestService{
		Name:        name,
		ComposeFile: "services/" + name + ".yml",
	}

	if err := h.ensureEmbeddedComposeFile(name); err != nil {
		return manifestService{}, err
	}
	if err := h.addServiceToManifestFile(svc); err != nil {
		return manifestService{}, err
	}
	return svc, nil
}

// ensureEmbeddedComposeFile writes the embedded compose file for name into
// ConfigDir/services/<name>.yml if it does not already exist. Mirrors
// cmd.ensureComposeFile (kept here to avoid a jobs -> cmd import).
//
// citadel#860: under an active CITADEL_NODE_DIR override, the materialized
// file's `container_name: citadel-<name>` line is rewritten to the
// override-namespaced name (citadel-<hash>-<name>), matching
// cmd.ensureComposeFile/cmd.embeddedContainerName -- both derive the name via
// compose.ContainerName from the SAME override-directory string, so the two
// materialization sites can never disagree. This reconciliation runs on
// EVERY call, including when the .yml already exists (see
// compose.EnsureNamespacedContainerName's doc for why the "already exists,
// leave it alone" fast path cannot skip it -- a stale unnamespaced file left
// in place is not just cosmetic drift, it's a `up` away from annexing the
// real node's global container name under the override's compose project).
// This package cannot see the --node-dir CLI flag (only cmd wires cobra
// flags), only the env var -- today that's moot in practice: every current
// constructor of ServiceHandler (cmd/work.go's runWork, cmd/hotswap.go) is
// unreachable while an override is active, because runWork refuses
// --node-dir/CITADEL_NODE_DIR outright before building any handlers (see
// cmd/work.go). This check is a defensive mirror of cmd.ensureComposeFile
// for when/if that refusal is ever narrowed to something override-compatible.
func (h *ServiceHandler) ensureEmbeddedComposeFile(name string) error {
	content, ok := embeddedservices.ServiceMap[name]
	if !ok {
		return fmt.Errorf("unknown embedded service: %s", name)
	}
	servicesDir := filepath.Join(h.ConfigDir, "services")
	destPath := filepath.Join(servicesDir, name+".yml")
	override := strings.TrimSpace(os.Getenv("CITADEL_NODE_DIR"))
	if _, err := os.Stat(destPath); err == nil {
		if override != "" {
			existing, err := os.ReadFile(destPath)
			if err != nil {
				return fmt.Errorf("read materialized compose file for %q: %w", name, err)
			}
			rewritten, changed, err := compose.EnsureNamespacedContainerName(string(existing), name, override)
			if err != nil {
				return fmt.Errorf("namespace container_name for %q under CITADEL_NODE_DIR override: %w", name, err)
			}
			if changed {
				if err := os.WriteFile(destPath, []byte(rewritten), 0600); err != nil {
					return fmt.Errorf("failed to rewrite compose file: %w", err)
				}
			}
		}
		// Ensure build-context aux files exist even if the .yml was written by
		// an older binary (idempotent no-op for image-based services).
		return embeddedservices.WriteAuxFiles(servicesDir, name)
	}
	if override != "" {
		rewritten, _, err := compose.EnsureNamespacedContainerName(content, name, override)
		if err != nil {
			return fmt.Errorf("namespace container_name for %q under CITADEL_NODE_DIR override: %w", name, err)
		}
		content = rewritten
	}
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return fmt.Errorf("failed to create services directory: %w", err)
	}
	// 0600 to protect any sensitive env vars, matching cmd.ensureComposeFile.
	if err := os.WriteFile(destPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}
	// Materialize any build-context files (e.g. bonsai's Dockerfile) so the
	// compose `build:` resolves on the node.
	return embeddedservices.WriteAuxFiles(servicesDir, name)
}

// addServiceToManifestFile appends a single service block to the citadel.yaml
// services list without disturbing the rest of the document. It operates on the
// raw yaml.Node tree (not the minimal serviceManifest struct) so that node:,
// capabilities:, and any operator-defined services survive the rewrite -- a
// struct round-trip would silently drop every field the minimal struct does not
// model. The operation is idempotent: if a service with the same name already
// exists, the file is left unchanged.
func (h *ServiceHandler) addServiceToManifestFile(svc manifestService) error {
	path := filepath.Join(h.ConfigDir, "citadel.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	// A well-formed citadel.yaml is a document node wrapping a mapping node.
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected manifest structure in %s", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("manifest root is not a mapping in %s", path)
	}

	// Locate (or create) the top-level "services" sequence.
	var servicesSeq *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "services" {
			servicesSeq = root.Content[i+1]
			break
		}
	}
	if servicesSeq == nil {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "services"}
		servicesSeq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, keyNode, servicesSeq)
	} else if servicesSeq.Kind != yaml.SequenceNode {
		// services: present but empty/null -- normalize to an empty sequence.
		servicesSeq.Kind = yaml.SequenceNode
		servicesSeq.Tag = "!!seq"
		servicesSeq.Value = ""
		servicesSeq.Content = nil
	}

	// Idempotency: bail if a service with this name is already registered.
	for _, item := range servicesSeq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(item.Content); j += 2 {
			if item.Content[j].Value == "name" && item.Content[j+1].Value == svc.Name {
				return nil
			}
		}
	}

	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: svc.Name},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "compose_file"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: svc.ComposeFile},
	)
	servicesSeq.Content = append(servicesSeq.Content, entry)

	// Encode with 2-space indent to match the citadel.yaml written by
	// `citadel init` (yaml.v3's default is 4), keeping the reconciled diff minimal.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return h.writeManifestBytes(path, buf.Bytes())
}

// setDesiredStatusInManifestFile sets (status == "stopped") or clears
// (status == "") the durable desired_status marker on a named service in
// citadel.yaml. It is the jobs-package counterpart of cmd/manifest.go's
// setServiceDesiredStatus (the jobs package cannot import cmd), implemented as
// yaml.Node surgery like addServiceToManifestFile so node:, capabilities:, and
// every field the minimal serviceManifest struct does not model survive the
// rewrite. Returns an error if the service is not present, so a caller does not
// silently no-op on a typo'd name.
func (h *ServiceHandler) setDesiredStatusInManifestFile(name, status string) error {
	path := filepath.Join(h.ConfigDir, "citadel.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected manifest structure in %s", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("manifest root is not a mapping in %s", path)
	}

	var servicesSeq *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "services" {
			servicesSeq = root.Content[i+1]
			break
		}
	}
	if servicesSeq == nil || servicesSeq.Kind != yaml.SequenceNode {
		return fmt.Errorf("service %q not found in manifest", name)
	}

	found := false
	for _, item := range servicesSeq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		isTarget := false
		statusIdx := -1
		for j := 0; j+1 < len(item.Content); j += 2 {
			switch item.Content[j].Value {
			case "name":
				if item.Content[j+1].Value == name {
					isTarget = true
				}
			case "desired_status":
				statusIdx = j
			}
		}
		if !isTarget {
			continue
		}
		found = true
		switch {
		case status == "" && statusIdx >= 0:
			// Clear: drop the key/value pair.
			item.Content = append(item.Content[:statusIdx], item.Content[statusIdx+2:]...)
		case status != "" && statusIdx >= 0:
			item.Content[statusIdx+1].Value = status
			item.Content[statusIdx+1].Tag = "!!str"
		case status != "":
			item.Content = append(item.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "desired_status"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: status},
			)
		}
		break
	}
	if !found {
		return fmt.Errorf("service %q not found in manifest", name)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return h.writeManifestBytes(path, buf.Bytes())
}

// setEvictedMarkersInManifestFile sets or clears the durable job-scoped
// reservation markers on a named service (citadel-cli#832, extends #577's
// plain desired_status): evicted_by_job records WHICH reservation stopped
// this service, so Release / ReconcileOrphanedReservations restore only what
// THIS reservation evicted — never a service an operator stopped for an
// unrelated reason (Execute()'s SERVICE_STOP/SERVICE_START clear these
// markers on any explicit operator action, for exactly that reason).
// evicted_prior_status records the service's desired_status value immediately
// before eviction, so a restore reinstates that exact prior durable intent
// instead of unconditionally clearing it.
//
// jobID == "" clears both markers (used by Release/reconcile once a restore
// succeeds, and by an explicit operator SERVICE_STOP/SERVICE_START). Like
// setDesiredStatusInManifestFile, this is yaml.Node surgery, not a struct
// round-trip, so node:, capabilities:, and any operator-defined fields survive
// the rewrite. Returns an error if the service is not present.
func (h *ServiceHandler) setEvictedMarkersInManifestFile(name, jobID, priorStatus string) error {
	path := filepath.Join(h.ConfigDir, "citadel.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("unexpected manifest structure in %s", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("manifest root is not a mapping in %s", path)
	}

	var servicesSeq *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "services" {
			servicesSeq = root.Content[i+1]
			break
		}
	}
	if servicesSeq == nil || servicesSeq.Kind != yaml.SequenceNode {
		return fmt.Errorf("service %q not found in manifest", name)
	}

	found := false
	for _, item := range servicesSeq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		isTarget := false
		for j := 0; j+1 < len(item.Content); j += 2 {
			if item.Content[j].Value == "name" && item.Content[j+1].Value == name {
				isTarget = true
				break
			}
		}
		if !isTarget {
			continue
		}
		found = true
		setManifestScalarField(item, "evicted_by_job", jobID)
		setManifestScalarField(item, "evicted_prior_status", priorStatus)
		break
	}
	if !found {
		return fmt.Errorf("service %q not found in manifest", name)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return h.writeManifestBytes(path, buf.Bytes())
}

// setManifestScalarField sets (value != "") or clears (value == "") a single
// scalar key on an already-located service mapping node, in place. Shared by
// setEvictedMarkersInManifestFile so the two reservation markers get identical
// set/clear/append handling without duplicating the node-surgery logic.
func setManifestScalarField(item *yaml.Node, key, value string) {
	idx := -1
	for j := 0; j+1 < len(item.Content); j += 2 {
		if item.Content[j].Value == key {
			idx = j
			break
		}
	}
	switch {
	case value == "" && idx >= 0:
		item.Content = append(item.Content[:idx], item.Content[idx+2:]...)
	case value != "" && idx >= 0:
		item.Content[idx+1].Value = value
		item.Content[idx+1].Tag = "!!str"
	case value != "":
		item.Content = append(item.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
		)
	}
}

func (h *ServiceHandler) findService(m *serviceManifest, name string) (manifestService, bool) {
	for _, s := range m.Services {
		if s.Name == name {
			return s, true
		}
	}
	return manifestService{}, false
}

func (h *ServiceHandler) knownServiceNames(m *serviceManifest) string {
	names := make([]string, len(m.Services))
	for i, s := range m.Services {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

func (h *ServiceHandler) resolveKind(svc manifestService) string {
	if svc.Type == "native" {
		return "native"
	}
	if svc.Type == "docker" {
		return "docker"
	}
	// Auto-detect: prefer native if available
	if services.IsNativeAvailable(svc.Name) {
		return "native"
	}
	return "docker"
}

// embeddedContainerNameFor mirrors cmd.embeddedContainerName for this
// package (citadel#860): "citadel-<svcName>" unchanged with no
// CITADEL_NODE_DIR override active, or the override-namespaced
// "citadel-<hash>-<svcName>" (compose.ContainerName, the SAME derivation
// cmd.embeddedContainerName and ensureEmbeddedComposeFile use) when one is --
// but ONLY for svcName values that are actually a services.ServiceMap entry.
// A payload/instance service name (e.g. a claudecode instance, containerNamePrefix
// in service_payload.go) is never in ServiceMap and keeps the plain
// "citadel-<svcName>" convention unconditionally, matching that this
// namespacing is scoped to EMBEDDED services only (citadel#860's non-goal).
func embeddedContainerNameFor(svcName string) string {
	if _, ok := embeddedservices.ServiceMap[svcName]; !ok {
		return "citadel-" + svcName
	}
	return compose.ContainerName(svcName, strings.TrimSpace(os.Getenv("CITADEL_NODE_DIR")))
}

func (h *ServiceHandler) isDockerServiceRunning(svcName string) bool {
	containerName := embeddedContainerNameFor(svcName)
	cmd := exec.Command("docker", "inspect", "--format", "{{.State.Status}}", containerName)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "running"
}

// dockerServiceEndpoint returns the reachable host endpoint (host:port) of a
// started docker service by inspecting the container's published port bindings,
// or "" if the container is absent or has no host port published. It reads
// NetworkSettings.Ports via `docker inspect` and delegates the parse to
// firstPublishedHostEndpoint so the mapping logic is unit-testable without
// Docker. The empty return is what lets serviceStart detect the #415 failure
// mode (a container that came up with NetworkSettings.Ports == {}).
func (h *ServiceHandler) dockerServiceEndpoint(svcName string) string {
	containerName := embeddedContainerNameFor(svcName)
	cmd := exec.Command("docker", "inspect",
		"--format", "{{json .NetworkSettings.Ports}}", containerName)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return firstPublishedHostEndpoint(out)
}

// dockerPortBinding mirrors an entry of docker inspect's
// NetworkSettings.Ports["<cport>/<proto>"] array.
type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// firstPublishedHostEndpoint parses the JSON of a container's
// NetworkSettings.Ports map and returns the first published host endpoint as
// "host:port". Container ports with no host binding (null value) are skipped,
// so a container with NetworkSettings.Ports == {} (or all-null) yields "".
// A "0.0.0.0"/"::"/empty HostIP is reported as 127.0.0.1 since the citadel
// gateway reaches services on loopback. Pure (bytes in, string out) so the
// #415 mapping assertion is testable without a live Docker daemon. To keep the
// choice deterministic across inspect's map ordering, the lowest host port is
// returned.
func firstPublishedHostEndpoint(portsJSON []byte) string {
	var ports map[string][]dockerPortBinding
	if err := json.Unmarshal(portsJSON, &ports); err != nil {
		return ""
	}
	bestHost := ""
	bestPort := 0
	for _, bindings := range ports {
		for _, b := range bindings {
			if b.HostPort == "" {
				continue
			}
			p, err := strconv.Atoi(b.HostPort)
			if err != nil || p <= 0 {
				continue
			}
			if bestPort != 0 && p >= bestPort {
				continue
			}
			host := b.HostIP
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			bestHost = host
			bestPort = p
		}
	}
	if bestPort == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", bestHost, bestPort)
}

// composeEnv returns the environment for docker compose invocations. It starts
// from the worker's own environment and guarantees CITADEL_WORKSPACE is set to
// the absolute workspace path. Compose files that bind-mount the workspace use
// ${CITADEL_WORKSPACE:?...}; without this, a worker started via --workspace (or
// the default path) has no CITADEL_WORKSPACE in its env and compose would fail.
func (h *ServiceHandler) composeEnv() []string {
	env := os.Environ()
	if h.WorkspaceDir != "" {
		// Override any inherited value so it always matches the workspace this
		// node actually writes job files into.
		env = append(env, "CITADEL_WORKSPACE="+h.WorkspaceDir)
	}
	// Supply the citadel-owned host ports so compose files that defer their host
	// publish to ${CITADEL_*_HOST_PORT} (llamacpp/vllm/extraction) resolve.
	env = append(env, embeddedservices.HostPortEnv()...)
	// Supply PUID/PGID = this node process's own uid/gid so PUID/PGID-aware module
	// containers (the meeting media stack) run as the node owner and write files
	// into bind-mounted workspace dirs owned by the node — no cross-UID perms
	// fixup. Harmless for compose files that don't reference them; guarded on
	// os.Getuid()>=0 so a non-POSIX host (Windows returns -1) never emits a bogus
	// PUID=-1 (the meeting stack is Linux-container-only anyway).
	if uid := os.Getuid(); uid >= 0 {
		env = append(env, fmt.Sprintf("PUID=%d", uid), fmt.Sprintf("PGID=%d", os.Getgid()))
	}
	return env
}

func (h *ServiceHandler) resolveComposePath(svc manifestService) (string, error) {
	if svc.ComposeFile == "" {
		return "", fmt.Errorf("service %s has no compose_file defined", svc.Name)
	}
	fullPath, err := platform.ValidatePathWithinDir(h.ConfigDir, svc.ComposeFile)
	if err != nil {
		return "", fmt.Errorf("invalid compose file path for %s: %w", svc.Name, err)
	}
	return fullPath, nil
}

func boolToStatus(running bool) string {
	if running {
		return "running"
	}
	return "stopped"
}
