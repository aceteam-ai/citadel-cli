// internal/jobs/service_handler.go
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/internal/compose"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"github.com/aceteam-ai/citadel-cli/internal/services"
	embedded "github.com/aceteam-ai/citadel-cli/services"
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
	// HostPorts are the published host ports declared by the started service's
	// compose file. Populated on a successful docker SERVICE_START so the caller
	// knows where the service is reachable on the node (citadel-cli#415).
	HostPorts []int `json:"host_ports,omitempty"`
}

func (h *ServiceHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	svcName := job.Payload["service"]
	if svcName == "" {
		return nil, fmt.Errorf("job payload missing 'service' field")
	}

	ctx.Log("info", "     - [Job %s] Service %s: %s", job.ID, job.Type, svcName)

	// Load manifest and validate service name against it.
	manifest, err := h.loadManifest()
	if err != nil {
		return nil, fmt.Errorf("failed to load manifest: %w", err)
	}

	svc, ok := h.findService(manifest, svcName)
	if !ok {
		// The service is not in the manifest. This is the common failure after a
		// binary upgrade adds a new embedded service (e.g. diffusers in v2.55.0):
		// the node advertises the capability in its heartbeat but its citadel.yaml,
		// written at init time, predates the service (citadel-cli#413). If the
		// requested service is one this binary embeds, materialize its compose from
		// the embedded template, persist an additive manifest block, and proceed --
		// so an on-demand SERVICE_START self-heals instead of being rejected. This
		// is the belt-and-suspenders companion to the startup manifest reconcile.
		reconciled, rErr := h.materializeEmbeddedService(svcName)
		if rErr != nil {
			return nil, fmt.Errorf("service %q not found in manifest (known: %s); "+
				"failed to materialize embedded service: %w",
				svcName, h.knownServiceNames(manifest), rErr)
		}
		if reconciled == nil {
			return nil, fmt.Errorf("service %q not found in manifest (known: %s)",
				svcName, h.knownServiceNames(manifest))
		}
		svc = *reconciled
		ctx.Log("info", "     - [Job %s] Materialized embedded service %s into manifest", job.ID, svcName)
	}

	switch job.Type {
	case "SERVICE_STATUS":
		return h.serviceStatus(svc)
	case "SERVICE_START":
		return h.serviceStart(ctx, svc)
	case "SERVICE_STOP":
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

func (h *ServiceHandler) serviceStart(ctx JobContext, svc manifestService) ([]byte, error) {
	kind := h.resolveKind(svc)
	var err error

	switch kind {
	case "native":
		if services.IsNativeServiceRunning(svc.Name) {
			return json.Marshal(serviceResult{
				Name: svc.Name, Running: true, Kind: kind,
				Action: "start", Message: svc.Name + " is already running",
			})
		}
		logDir := filepath.Join(h.ConfigDir, "logs")
		_, err = services.StartNativeService(svc.Name, logDir)

	case "docker":
		if h.isDockerServiceRunning(svc.Name) {
			return json.Marshal(serviceResult{
				Name: svc.Name, Running: true, Kind: kind,
				Action: "start", Message: svc.Name + " is already running",
			})
		}
		composePath, pathErr := h.resolveComposePath(svc)
		if pathErr != nil {
			return nil, pathErr
		}
		// Include the least-privilege sandbox override when present (untrusted/
		// Tier-2 modules) so a remotely-started module also runs hardened -- the
		// override would otherwise be bypassed by this start site.
		composeArgs := []string{"compose", "-f", composePath}
		if override := catalog.ExistingSandboxOverride(filepath.Dir(composePath),
			strings.TrimSuffix(filepath.Base(composePath), filepath.Ext(filepath.Base(composePath)))); override != "" {
			composeArgs = append(composeArgs, "-f", override)
		}
		// --force-recreate guarantees the container is (re)created from THIS compose
		// definition. Without it, a stale citadel-<svc> container left from a prior
		// run with a different (or no) port mapping is reused, and the container
		// comes up with NetworkSettings.Ports == {} -- a healthy server on its
		// internal contract port is then unreachable on the host (citadel-cli#415).
		// We only reach here when the service is not already running (checked
		// above), so recreating is safe.
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

	result := serviceResult{
		Name: svc.Name, Running: true, Kind: kind,
		Action: "start", Message: fmt.Sprintf("%s started successfully", svc.Name),
	}
	// Surface the compose-declared host ports so the caller knows where the
	// service is reachable on the node (citadel-cli#415). Docker-kind only; native
	// services manage their own listen port.
	if kind == "docker" {
		result.HostPorts = h.composeHostPorts(svc)
	}
	return json.Marshal(result)
}

// composeHostPorts returns the published host ports declared in a service's
// compose file, or nil if the file cannot be read/parsed. Used to report the
// reachable endpoint after a successful SERVICE_START (citadel-cli#415).
func (h *ServiceHandler) composeHostPorts(svc manifestService) []int {
	composePath, err := h.resolveComposePath(svc)
	if err != nil {
		return nil
	}
	content, err := os.ReadFile(composePath)
	if err != nil {
		return nil
	}
	return compose.HostPorts(content)
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
		cmd := exec.Command("docker", "compose", "-f", composePath, "down")
		cmd.Env = h.composeEnv()
		out, cmdErr := cmd.CombinedOutput()
		if cmdErr != nil {
			err = fmt.Errorf("docker compose down failed: %s", strings.TrimSpace(string(out)))
		}
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

// serviceNameOK guards against path traversal via the service name before it is
// used to build a compose file path. It mirrors the config handler's allowlist:
// lowercase alphanumerics and hyphens only.
func serviceNameOK(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// materializeEmbeddedService self-heals a manifest that predates an embedded
// service (citadel-cli#413). If svcName is one this binary embeds
// (embedded.ServiceMap), it writes the embedded compose to
// <ConfigDir>/services/<name>.yml (without overwriting an existing file),
// persists an additive service block into citadel.yaml (preserving all other
// manifest content), and returns the resolved manifestService. If svcName is not
// an embedded service it returns (nil, nil) so the caller emits the normal
// "not found in manifest" error.
func (h *ServiceHandler) materializeEmbeddedService(svcName string) (*manifestService, error) {
	if !serviceNameOK(svcName) {
		return nil, nil
	}
	content, ok := embedded.ServiceMap[svcName]
	if !ok {
		return nil, nil // not an embedded service -- caller emits normal error
	}

	// Write the embedded compose file if it is not already present. An existing
	// (operator-authored) file is left untouched.
	servicesDir := filepath.Join(h.ConfigDir, "services")
	composeFile := filepath.Join(servicesDir, svcName+".yml")
	if _, err := os.Stat(composeFile); os.IsNotExist(err) {
		if err := os.MkdirAll(servicesDir, 0o755); err != nil {
			return nil, fmt.Errorf("create services dir: %w", err)
		}
		// 0600 to protect any sensitive env vars, matching the config handler.
		if err := os.WriteFile(composeFile, []byte(content), 0o600); err != nil {
			return nil, fmt.Errorf("write compose file: %w", err)
		}
	}

	relCompose := filepath.Join("./services", svcName+".yml")
	svc := manifestService{Name: svcName, ComposeFile: relCompose}

	// Persist the additive manifest block, preserving every other field.
	if err := h.appendManifestService(svc); err != nil {
		return nil, fmt.Errorf("persist manifest block: %w", err)
	}
	return &svc, nil
}

// appendManifestService adds a service block to citadel.yaml without disturbing
// any other content (node, capabilities, config, other services). It performs a
// generic YAML read-modify-write so unknown top-level keys survive. It is a no-op
// when the service is already present.
func (h *ServiceHandler) appendManifestService(svc manifestService) error {
	path := filepath.Join(h.ConfigDir, "citadel.yaml")

	var doc map[string]any
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parse manifest: %w", err)
		}
	}
	if doc == nil {
		doc = make(map[string]any)
	}

	var svcList []any
	if existing, ok := doc["services"].([]any); ok {
		svcList = existing
	}
	// Idempotency: bail if a service with this name already exists.
	for _, entry := range svcList {
		if m, ok := entry.(map[string]any); ok {
			if name, _ := m["name"].(string); name == svc.Name {
				return nil
			}
		}
	}

	svcList = append(svcList, map[string]any{
		"name":         svc.Name,
		"compose_file": svc.ComposeFile,
	})
	doc["services"] = svcList

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
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
