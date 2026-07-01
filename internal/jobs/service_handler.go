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
		composeArgs = append(composeArgs, "up", "-d")
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

	return json.Marshal(serviceResult{
		Name: svc.Name, Running: true, Kind: kind,
		Action: "start", Message: fmt.Sprintf("%s started successfully", svc.Name),
	})
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

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0600)
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
