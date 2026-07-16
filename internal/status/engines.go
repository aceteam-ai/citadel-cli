package status

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	nativesvc "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// idleCapableEngines lists the serving engines for which idle detection has a
// reliable request signal. Currently only vLLM exposes the Prometheus request
// counters + running/waiting gauges the IdleTracker scrapes. Extend this as
// other engines (sglang, llama.cpp) grow comparable metrics.
var idleCapableEngines = []string{"vllm"}

// managedProbeEngines lists the managed serving engines the heartbeat path
// probes for a live signal: an idle signal (idleCapableEngines) and/or the
// loaded model(s) over the engine's local HTTP API (#529). It must remain a
// superset of idleCapableEngines (guarded by a test) so extending the idle
// list never silently drops an engine from the heartbeat.
var managedProbeEngines = []string{"vllm", "ollama", "llamacpp"}

// collectManagedEngineStatus reports running managed serving engines (from the
// embedded services.ServiceMap) so their telemetry reaches the heartbeat even
// when no manifest-driven service config was passed to the collector (the
// common heartbeat case, where c.services is nil). Each entry carries the
// per-service idle signal when the engine's metrics are scrapeable (citadel
// #416) and the currently LOADED model(s) discovered from the engine's local
// API (citadel #529) — e.g. vLLM/llama.cpp `GET /v1/models`, ollama
// `GET /api/ps`.
//
// It only emits an entry for an engine that is actually running AND answered
// at least one probe (idle scrape or model discovery). An engine that answers
// model discovery with an empty list (llama.cpp up with no model loaded,
// ollama with nothing resident) IS emitted — running with no models is real,
// reportable state. A running-but-unresponsive engine (e.g. still loading its
// model, HTTP not yet up) is skipped rather than reported with a misleading
// "idle since startup" — the existing manifest-driven collectServiceStatus
// still reports its up/down state when configured. Probe failures never fail
// the collection: each probe is bounded by ModelDiscoveryTimeout and a failure
// simply leaves the corresponding field empty.
func (c *Collector) collectManagedEngineStatus() []ServiceInfo {
	ctx := context.Background()
	var out []ServiceInfo

	// Resolve the container runtime once and use its engine binary for the
	// running-check, mirroring the start path (cmd/service.go). GPU/inference
	// containers are the ones most likely to run under the hardened podman
	// runtime (#348); a docker-only inspect would miss them entirely and the
	// idle signal would never fire on exactly the nodes this feature targets.
	engineBin := catalog.SelectContainerRuntime().EngineBin

	for _, name := range managedProbeEngines {
		port, running := managedEnginePortIfRunning(engineBin, name)
		if !running || port <= 0 {
			continue
		}

		info := ServiceInfo{
			Name:   name,
			Type:   ServiceTypeLLM,
			Status: ServiceStatusRunning,
			Port:   port,
			Health: HealthStatusOK,
		}
		responded := false

		if c.idleTracker != nil && engineInList(idleCapableEngines, name) {
			if state, ok := c.idleTracker.Observe(ctx, name, name, port); ok {
				idle := state
				info.IdleState = &idle
				responded = true
			}
		}

		if c.modelDiscovery != nil {
			mctx, cancel := context.WithTimeout(ctx, ModelDiscoveryTimeout)
			models, err := c.modelDiscovery.DiscoverModels(mctx, name, port)
			cancel()
			if err == nil {
				info.Models = models
				responded = true
			}
		}

		if !responded {
			continue
		}
		out = append(out, info)
	}
	return out
}

// engineInList reports whether name is present in the given engine list.
func engineInList(list []string, name string) bool {
	for _, e := range list {
		if e == name {
			return true
		}
	}
	return false
}

// managedEnginePortIfRunning reports whether a managed engine is running and,
// if so, its host port. It checks the container "citadel-<name>" first (the
// compose deploy path) using the given engine binary (docker or podman), and
// falls back to the native process check. The host port is resolved from the
// embedded compose file's port mapping, falling back to the known native
// default.
func managedEnginePortIfRunning(engineBin, name string) (port int, running bool) {
	if containerRunning(engineBin, "citadel-"+name) {
		return managedEngineHostPort(name), true
	}
	if nativesvc.IsNativeServiceRunning(name) {
		if p, ok := nativesvc.GetServicePort(name); ok {
			return p, true
		}
		return managedEngineHostPort(name), true
	}
	return 0, false
}

// containerRunning reports whether a container with the given name is in the
// "running" state, using the provided engine binary (docker or podman). Any
// error (runtime not installed, container absent) yields false so callers treat
// the engine as not-managed-here.
func containerRunning(engineBin, containerName string) bool {
	if engineBin == "" {
		engineBin = "docker"
	}
	cmd := exec.Command(engineBin, "inspect", "--format", "{{.State.Status}}", containerName)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "running"
}

// composePortRe matches the host side of a compose short-form port mapping,
// e.g. "8100:8000" or "127.0.0.1:8100:8000" -> host port 8100.
var composePortRe = regexp.MustCompile(`(?:\d+\.\d+\.\d+\.\d+:)?(\d+):\d+`)

// managedEngineHostPort resolves the published host port for a managed engine.
// For engines whose host publish citadel owns via ${CITADEL_*_HOST_PORT}
// substitution (llamacpp/vllm/extraction/diffusers), the compose file no longer
// carries a literal host port, so the port comes from the registry
// (services/ports.go). For any other engine it falls back to parsing the first
// port mapping of its embedded compose file. Returns 0 when neither yields a
// port.
func managedEngineHostPort(name string) int {
	if port, ok := services.ManagedServiceHostPort(name); ok {
		return port
	}
	compose, ok := services.ServiceMap[name]
	if !ok {
		return 0
	}
	return firstComposeHostPort(compose)
}

// firstComposeHostPort parses a compose document and returns the host port of
// the first service's first port mapping, or 0 if none is found.
func firstComposeHostPort(composeYAML string) int {
	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeYAML), &doc); err != nil {
		return 0
	}
	for _, svc := range doc.Services {
		for _, p := range svc.Ports {
			if m := composePortRe.FindStringSubmatch(p); m != nil {
				if hp, err := strconv.Atoi(m[1]); err == nil {
					return hp
				}
			}
		}
	}
	return 0
}
