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

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/services"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// serviceManifest is a minimal subset of the citadel.yaml manifest used by
// the service handler.  It lives here (not in cmd/) to avoid import cycles.
type serviceManifest struct {
	Services []manifestService `yaml:"services"`
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
		// Optional model selection (#530): the backend's model-deploy contract
		// dispatches MODEL_CACHE_PULL (weights) then SERVICE_START
		// {service, model}. The model, when present, is persisted per-service
		// and injected into the engine's compose interpolation env.
		return h.serviceStart(ctx, svc, job.Payload["model"])
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
		running = services.IsNativeServiceRunning(svc.Name)
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
func (h *ServiceHandler) serviceStart(ctx JobContext, svc manifestService, model string) ([]byte, error) {
	kind := h.resolveKind(svc)
	var err error
	// appliedModel is the model this start serves via compose env interpolation;
	// empty when no model was requested or the engine takes none.
	appliedModel := ""

	switch kind {
	case "native":
		if model != "" {
			ctx.Log("info", "     - Service %s runs natively; model %q ignored (no compose env to inject)", svc.Name, model)
		}
		if services.IsNativeServiceRunning(svc.Name) {
			return json.Marshal(serviceResult{
				Name: svc.Name, Running: true, Kind: kind,
				Action: "start", Message: svc.Name + " is already running",
			})
		}
		logDir := filepath.Join(h.ConfigDir, "logs")
		_, err = services.StartNativeService(svc.Name, logDir)

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
		// Already-running short-circuit, UNLESS the persisted model just changed:
		// then the running container serves the old model and must be recreated.
		if !modelChanged && h.isDockerServiceRunning(svc.Name) {
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
		cmd := exec.Command("docker", composeArgs...)
		cmd.Env = h.composeEnv()
		out, cmdErr := cmd.CombinedOutput()
		if cmdErr != nil {
			err = fmt.Errorf("docker compose up failed: %s", strings.TrimSpace(string(out)))
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
func (h *ServiceHandler) ensureEmbeddedComposeFile(name string) error {
	content, ok := embeddedservices.ServiceMap[name]
	if !ok {
		return fmt.Errorf("unknown embedded service: %s", name)
	}
	servicesDir := filepath.Join(h.ConfigDir, "services")
	destPath := filepath.Join(servicesDir, name+".yml")
	if _, err := os.Stat(destPath); err == nil {
		return nil // already materialized
	}
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return fmt.Errorf("failed to create services directory: %w", err)
	}
	// 0600 to protect any sensitive env vars, matching cmd.ensureComposeFile.
	if err := os.WriteFile(destPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}
	return nil
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
	return os.WriteFile(path, buf.Bytes(), 0600)
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
	return os.WriteFile(path, buf.Bytes(), 0600)
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

func (h *ServiceHandler) isDockerServiceRunning(svcName string) bool {
	containerName := "citadel-" + svcName
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
	containerName := "citadel-" + svcName
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
