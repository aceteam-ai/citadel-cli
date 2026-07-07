// internal/jobs/service_payload.go
//
// Extended SERVICE_START handling: launch an agent-runtime container directly
// from an inline job payload (image / env / host_port / state volume) instead
// of resolving a service NAME against the node's citadel.yaml or the embedded
// ServiceMap. This is the node-side receiver (Child C) for the BYOC agent-host
// launch contract the platform dispatches (aceteam#4588 / #4590,
// aceteam/python-backend/utils/instance_node_dispatch.py: NodeServiceSpec).
//
// Contract (what the platform actually sends, verified in aceteam#5134):
//
//	{
//	  "service":           "ac-<shortcode>",             // container name suffix + STOP/STATUS key
//	  "instance_id":       "<uuid>",
//	  "image":             "ghcr.io/aceteam-ai/claudecode-service:latest",
//	  "env":               {"ANTHROPIC_BASE_URL": "...", ...},  // JSON object
//	  "host_port":         18789,                        // constant today (NOT per-instance)
//	  "state_volume_path": "~/citadel-cache/instances/<id>",   // literal ~, node-expanded
//	  "state_mount_path":  "/state"
//	}
//
// The platform does NOT read back the launched endpoint; it reaches the
// container at host_port over the mesh. So the host port is published VERBATIM,
// never re-allocated. The env carries no PORT or CLAUDE_CONFIG_DIR, so this
// handler injects both: PORT so the wrapper listens on the port we publish, and
// CLAUDE_CONFIG_DIR = state_mount_path so Claude Code writes its state INTO the
// mounted per-instance volume (the entrypoint chowns CLAUDE_CONFIG_DIR
// generically, so durability works without a container change).
package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
	embeddedservices "github.com/aceteam-ai/citadel-cli/services"
)

// containerNamePrefix is the docker container-name prefix shared with the
// manifest path (isDockerServiceRunning / dockerServiceEndpoint both look up
// "citadel-<service>"), so those helpers work unchanged for payload instances.
const containerNamePrefix = "citadel-"

// defaultStateMountPath mirrors the platform's DEFAULT_STATE_MOUNT_PATH. Used
// only when the payload omits state_mount_path.
const defaultStateMountPath = "/state"

// defaultContainerPort is the wrapper's fixed internal contract port
// (services/claudecode-service/wrapper.py PORT default). Used when the payload
// env carries no PORT of its own.
const defaultContainerPort = 8787

// allowedImageRegistryPrefixes restricts payload-launched images to AceTeam's
// official registry namespace. This mirrors the trust model in internal/catalog
// (only operator-trusted sources may run): the payload arrives over the
// authenticated per-node job stream, but an image reference is still a
// code-execution grant, so it is constrained to the namespace that publishes
// the vetted BYOC runtime images (claudecode-service, and future agent
// runtimes). Widen this deliberately, not implicitly.
var allowedImageRegistryPrefixes = []string{
	"ghcr.io/aceteam-ai/",
}

// instanceSpec is the validated, node-resolved form of an extended
// SERVICE_START payload, ready to launch.
type instanceSpec struct {
	ServiceName   string
	InstanceID    string
	ContainerName string
	Image         string
	Env           map[string]string
	HostPort      int
	ContainerPort int
	// StateVolumePath is the absolute, ~-expanded, validated host path.
	StateVolumePath string
	StateMountPath  string
}

// payloadHasInlineSpec reports whether a job payload is the extended,
// self-contained launch form (carries an "image") rather than the legacy
// name-only {"service": name} form. This is the switch that keeps the existing
// manifest/embedded path untouched: only an image-bearing SERVICE_START takes
// the payload-launch branch.
func payloadHasInlineSpec(payload map[string]string) bool {
	return strings.TrimSpace(payload["image"]) != ""
}

// parseInstanceSpec validates an extended SERVICE_START payload and resolves it
// into a launchable instanceSpec. homeDir is the node owner's home directory
// (injected for testability); it is used to expand a leading "~" in the state
// volume path and to bound that path to the citadel data dir.
//
// Pure apart from the passed-in homeDir: it performs no I/O, so it is
// table-testable without Docker or a filesystem.
func parseInstanceSpec(payload map[string]string, homeDir string) (*instanceSpec, error) {
	serviceName := strings.TrimSpace(payload["service"])
	if serviceName == "" {
		return nil, fmt.Errorf("payload missing 'service' name")
	}
	if err := validateServiceName(serviceName); err != nil {
		return nil, err
	}

	image := strings.TrimSpace(payload["image"])
	if err := validateImageRef(image); err != nil {
		return nil, err
	}

	env, err := parseEnvField(payload["env"])
	if err != nil {
		return nil, err
	}

	hostPort, err := parsePort(payload["host_port"])
	if err != nil {
		return nil, fmt.Errorf("invalid host_port: %w", err)
	}
	if err := validateHostPort(hostPort); err != nil {
		return nil, err
	}

	mountPath := strings.TrimSpace(payload["state_mount_path"])
	if mountPath == "" {
		mountPath = defaultStateMountPath
	}
	if !strings.HasPrefix(mountPath, "/") {
		return nil, fmt.Errorf("state_mount_path %q must be an absolute container path", mountPath)
	}

	volPath, err := resolveStateVolumePath(payload["state_volume_path"], homeDir)
	if err != nil {
		return nil, err
	}

	// Container port: honor an explicit PORT in the payload env, else the
	// wrapper's default. The host port is published to this in-container port.
	containerPort := defaultContainerPort
	if p, ok := env["PORT"]; ok {
		parsed, perr := parsePort(p)
		if perr != nil {
			return nil, fmt.Errorf("invalid env PORT: %w", perr)
		}
		containerPort = parsed
	}

	return &instanceSpec{
		ServiceName:     serviceName,
		InstanceID:      strings.TrimSpace(payload["instance_id"]),
		ContainerName:   containerNamePrefix + serviceName,
		Image:           image,
		Env:             env,
		HostPort:        hostPort,
		ContainerPort:   containerPort,
		StateVolumePath: volPath,
		StateMountPath:  mountPath,
	}, nil
}

// validateServiceName rejects names that could break out of the "citadel-<name>"
// container-name arg or a docker flag position. Allows the platform's
// "ac-<shortcode>" shape (alphanumerics, dash, underscore, dot).
func validateServiceName(name string) error {
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid service name %q: must not start with '-'", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("invalid service name %q: only [A-Za-z0-9._-] allowed", name)
		}
	}
	return nil
}

// validateImageRef enforces the registry allowlist and rejects a reference that
// could be read as a docker flag.
func validateImageRef(image string) error {
	if image == "" {
		return fmt.Errorf("payload missing 'image'")
	}
	if strings.HasPrefix(image, "-") {
		return fmt.Errorf("invalid image %q: must not start with '-'", image)
	}
	for _, prefix := range allowedImageRegistryPrefixes {
		if strings.HasPrefix(image, prefix) {
			return nil
		}
	}
	return fmt.Errorf("image %q is not from an allowed registry (allowed: %s)",
		image, strings.Join(allowedImageRegistryPrefixes, ", "))
}

// validateHostPort keeps a payload from publishing on a port citadel's own
// listeners own (gateway, status server, VNC, terminal, ...). The host-port
// registry (services/ports.go) is used here for VALIDATION only -- the port is
// assigned by the platform, not re-allocated -- so a payload can never collide
// with a citadel-internal listener.
func validateHostPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("host_port %d out of range", port)
	}
	if name, reserved := embeddedservices.ReservedCitadelPorts[port]; reserved {
		return fmt.Errorf("host_port %d is reserved for citadel's %s listener", port, name)
	}
	return nil
}

// parseEnvField decodes the "env" payload field. The worker->nexus.Job adapter
// json-encodes non-scalar payload values (see internal/worker/handler_adapter.go),
// so env arrives here as a JSON object string. An empty field is an empty env.
func parseEnvField(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]string{}, nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("invalid env payload (expected JSON object of string->string): %w", err)
	}
	for k := range env {
		if strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("env contains an empty key")
		}
	}
	return env, nil
}

// parsePort parses a port that may arrive as an int-valued string (e.g. "18789")
// or, defensively, a float-valued one from a JSON number.
func parsePort(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty")
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", raw)
	}
	return int(f), nil
}

// resolveStateVolumePath expands a leading "~" to homeDir, makes the path
// absolute, and bounds it to a citadel data dir. Docker's -v does NOT expand
// "~" (unlike compose), so the node must do it. Bounding it to
// <home>/citadel-cache or <home>/.citadel rejects a host-path mount that would
// escape the citadel data area (e.g. "/etc" or "~/.ssh").
func resolveStateVolumePath(raw, homeDir string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("payload missing 'state_volume_path'")
	}
	if homeDir == "" {
		return "", fmt.Errorf("cannot resolve state_volume_path: home directory unknown")
	}

	expanded := raw
	switch {
	case raw == "~":
		expanded = homeDir
	case strings.HasPrefix(raw, "~/"):
		expanded = filepath.Join(homeDir, raw[2:])
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("failed to resolve state_volume_path %q: %w", raw, err)
	}
	abs = filepath.Clean(abs)

	allowedBases := []string{
		filepath.Join(homeDir, "citadel-cache"),
		filepath.Join(homeDir, ".citadel"),
	}
	for _, base := range allowedBases {
		if abs == base || strings.HasPrefix(abs, base+string(filepath.Separator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf(
		"state_volume_path %q resolves to %q, which is outside the citadel data dir (allowed: %s)",
		raw, abs, strings.Join(allowedBases, ", "))
}

// buildDockerRunArgs assembles the `docker run` arguments for a spec. Pure
// (spec in, args out) so the port/volume/env wiring is testable without Docker.
// The image is placed last, after all flags, so it can never be read as a flag.
func buildDockerRunArgs(spec *instanceSpec) []string {
	args := []string{
		"run", "-d",
		"--name", spec.ContainerName,
		// Survive node reboot/upgrade with no citadel loop (nothing else
		// auto-starts these). STOP does an explicit `docker rm`, so this does
		// not resurrect a deliberately stopped instance.
		"--restart", "unless-stopped",
		// Publish on loopback only: the citadel gateway/mesh reaches services on
		// 127.0.0.1, matching the manifest services' host publish.
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", spec.HostPort, spec.ContainerPort),
		"-v", fmt.Sprintf("%s:%s", spec.StateVolumePath, spec.StateMountPath),
	}

	// Inject the durability + port env unless the payload already set them, so
	// the runtime writes into the mounted volume and listens on the published
	// port. Explicit payload values win.
	injected := map[string]string{
		"CLAUDE_CONFIG_DIR": spec.StateMountPath,
		"PORT":              strconv.Itoa(spec.ContainerPort),
	}
	for k, v := range injected {
		if _, ok := spec.Env[k]; !ok {
			args = append(args, "-e", k+"="+v)
		}
	}

	// Env passed as-is. Sorted for deterministic arg order (stable tests, stable
	// logs); docker does not care about ordering.
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "-e", k+"="+spec.Env[k])
	}

	args = append(args, spec.Image)
	return args
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Simple insertion sort avoids pulling in sort for a tiny map and keeps the
	// dependency surface minimal; env maps are small.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// Handler entry points (payload-launched instances)
// ---------------------------------------------------------------------------

// serviceStartPayload launches (or reports already-running) a container from an
// extended SERVICE_START payload and records it in the instance store so
// SERVICE_STOP / SERVICE_STATUS can find it later.
func (h *ServiceHandler) serviceStartPayload(ctx JobContext, job *nexus.Job) ([]byte, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve home directory for payload launch: %w", err)
	}
	spec, err := parseInstanceSpec(job.Payload, homeDir)
	if err != nil {
		return nil, fmt.Errorf("invalid SERVICE_START payload: %w", err)
	}

	ctx.Log("info", "     - [Job %s] Launching instance %s from payload (image=%s, host_port=%d)",
		job.ID, spec.ServiceName, spec.Image, spec.HostPort)

	// Idempotent start: if the container is already running, report it (and keep
	// the store in sync) rather than failing on a duplicate name.
	if h.isDockerServiceRunning(spec.ServiceName) {
		if err := h.recordInstance(spec); err != nil {
			ctx.Log("warning", "     - [Job %s] instance %s running but store update failed: %v", job.ID, spec.ServiceName, err)
		}
		return h.instanceResult(spec, "start", true, "")
	}

	// Ensure the state volume dir exists on the host before mounting. The
	// container entrypoint chowns it to the runtime user, so host ownership is
	// fine; it just has to exist.
	if err := os.MkdirAll(spec.StateVolumePath, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state volume dir %s: %w", spec.StateVolumePath, err)
	}

	// A stale stopped container with the same name would block `docker run
	// --name`; remove it first (best-effort).
	_ = exec.Command("docker", "rm", "-f", spec.ContainerName).Run()

	cmd := exec.Command("docker", buildDockerRunArgs(spec)...)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return json.Marshal(serviceResult{
			Name: spec.ServiceName, Running: false, Kind: "docker",
			Action: "start", Error: strings.TrimSpace(string(out)),
			Message: fmt.Sprintf("failed to launch %s: %s", spec.ServiceName, strings.TrimSpace(string(out))),
		})
	}

	if err := h.recordInstance(spec); err != nil {
		// The container is up; a store write failure must not report failure to
		// the platform, but it does mean STOP/STATUS may miss it. Surface it.
		ctx.Log("warning", "     - [Job %s] launched %s but failed to persist instance record: %v",
			job.ID, spec.ServiceName, err)
	}

	return h.instanceResult(spec, "start", true, "")
}

// serviceStatusPayload reports the status of a payload-launched instance.
func (h *ServiceHandler) serviceStatusPayload(rec InstanceRecord) ([]byte, error) {
	running := h.isDockerServiceRunning(rec.ServiceName)
	return json.Marshal(serviceResult{
		Name:    rec.ServiceName,
		Running: running,
		Kind:    "docker",
		Action:  "status",
		Message: fmt.Sprintf("%s is %s (docker)", rec.ServiceName, boolToStatus(running)),
	})
}

// serviceStopPayload stops and removes a payload-launched instance's container
// and drops it from the instance store. The state volume on disk is left
// intact so a later start reattaches the same durable state.
func (h *ServiceHandler) serviceStopPayload(ctx JobContext, rec InstanceRecord) ([]byte, error) {
	if h.isDockerServiceRunning(rec.ServiceName) || h.dockerContainerExists(rec.ContainerName) {
		cmd := exec.Command("docker", "rm", "-f", rec.ContainerName)
		if out, err := cmd.CombinedOutput(); err != nil {
			return json.Marshal(serviceResult{
				Name: rec.ServiceName, Running: h.isDockerServiceRunning(rec.ServiceName), Kind: "docker",
				Action: "stop", Error: strings.TrimSpace(string(out)),
				Message: fmt.Sprintf("failed to stop %s: %s", rec.ServiceName, strings.TrimSpace(string(out))),
			})
		}
	}
	if err := h.instanceStore().Delete(rec.ServiceName); err != nil {
		ctx.Log("warning", "     - instance %s stopped but store cleanup failed: %v", rec.ServiceName, err)
	}
	return json.Marshal(serviceResult{
		Name: rec.ServiceName, Running: false, Kind: "docker",
		Action: "stop", Message: fmt.Sprintf("%s stopped successfully", rec.ServiceName),
	})
}

// instanceResult builds the success result for a payload instance, including
// the reachable endpoint derived from the published port.
func (h *ServiceHandler) instanceResult(spec *instanceSpec, action string, running bool, errMsg string) ([]byte, error) {
	res := serviceResult{
		Name: spec.ServiceName, Running: running, Kind: "docker",
		Action: action, Error: errMsg,
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", spec.HostPort)
	res.Endpoint = endpoint
	if running {
		res.Message = fmt.Sprintf("%s started successfully; reachable at %s", spec.ServiceName, endpoint)
	} else {
		res.Message = fmt.Sprintf("%s is not running", spec.ServiceName)
	}
	return json.Marshal(res)
}

// recordInstance persists a launched instance's spec.
func (h *ServiceHandler) recordInstance(spec *instanceSpec) error {
	return h.instanceStore().Put(InstanceRecord{
		ServiceName:     spec.ServiceName,
		InstanceID:      spec.InstanceID,
		ContainerName:   spec.ContainerName,
		Image:           spec.Image,
		HostPort:        spec.HostPort,
		ContainerPort:   spec.ContainerPort,
		StateVolumePath: spec.StateVolumePath,
		StateMountPath:  spec.StateMountPath,
	})
}

// instanceStore returns the handler's instance store, creating it on first use.
func (h *ServiceHandler) instanceStore() *instanceStore {
	if h.instances == nil {
		s, err := newInstanceStore()
		if err != nil {
			// A store we cannot open degrades to a no-op-ish store rooted at a
			// path that will error on write; callers log the error. Returning a
			// zero-value store keeps the nil-check simple.
			h.instances = &instanceStore{}
			_ = err
		} else {
			h.instances = s
		}
	}
	return h.instances
}

// dockerContainerExists reports whether a container with the given name exists
// in any state (running or exited), so STOP can remove a stopped instance.
func (h *ServiceHandler) dockerContainerExists(containerName string) bool {
	cmd := exec.Command("docker", "inspect", "--format", "{{.Name}}", containerName)
	return cmd.Run() == nil
}
