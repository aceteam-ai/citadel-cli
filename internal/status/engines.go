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

// collectManagedEngineStatus reports running managed serving engines (from the
// embedded services.ServiceMap) that carry an idle signal, so the per-service
// idle telemetry reaches the heartbeat even when no manifest-driven service
// config was passed to the collector (the common heartbeat case, where
// c.services is nil).
//
// It only emits an entry for an engine that is actually running AND whose
// metrics endpoint could be scraped. A running-but-unscrapeable engine (e.g.
// still loading its model, /metrics not yet up) is skipped rather than reported
// with a misleading "idle since startup" — the existing manifest-driven
// collectServiceStatus still reports its up/down state when configured.
func (c *Collector) collectManagedEngineStatus() []ServiceInfo {
	if c.idleTracker == nil {
		return nil
	}
	ctx := context.Background()
	var out []ServiceInfo

	// Resolve the container runtime once and use its engine binary for the
	// running-check, mirroring the start path (cmd/service.go). GPU/inference
	// containers are the ones most likely to run under the hardened podman
	// runtime (#348); a docker-only inspect would miss them entirely and the
	// idle signal would never fire on exactly the nodes this feature targets.
	engineBin := catalog.SelectContainerRuntime().EngineBin

	for _, name := range idleCapableEngines {
		port, running := managedEnginePortIfRunning(engineBin, name)
		if !running || port <= 0 {
			continue
		}
		state, ok := c.idleTracker.Observe(ctx, name, name, port)
		if !ok {
			continue
		}
		idle := state
		out = append(out, ServiceInfo{
			Name:      name,
			Type:      ServiceTypeLLM,
			Status:    ServiceStatusRunning,
			Port:      port,
			Health:    HealthStatusOK,
			IdleState: &idle,
		})
	}
	return out
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

// managedEngineHostPort resolves the published host port for a managed engine by
// parsing the first port mapping of its embedded compose file. Returns 0 when
// the compose file is absent or has no parseable port mapping.
func managedEngineHostPort(name string) int {
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
