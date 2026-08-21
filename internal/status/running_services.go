package status

import (
	"os/exec"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
	"github.com/aceteam-ai/citadel-cli/services"
)

// running_services.go: "report what is ACTUALLY running", the node side of
// aceteam#7148.
//
// Before this, the heartbeat only ever named a service from one of three narrow
// sources: the manifest-driven service list (nil on the heartbeat path), the
// five engines in managedProbeEngines, and the one service in
// embeddingProbeServices. A service materialized by SERVICE_START straight from
// the embedded services.ServiceMap (the path Quick Deploy and the per-node
// Deploy tab use) was therefore invisible unless it happened to be one of those
// six AND answered a probe. Deploying TEI to node 1314 left `service_status`
// saying "tei is running (docker)" while `fabric_node_status` said "Running
// Services: none", and every embedded compose engine other than TEI (kokoro,
// transcribe, diffusers, extraction, sglang, lmstudio) had no path into the
// inventory at all.
//
// The fix is to enumerate the running containers and report them, so the
// inventory answers "what is up" rather than "what did we think to ask about".

// citadelContainerPrefix is the container_name prefix every embedded compose
// service uses (services/compose/*.yml all set `container_name: citadel-<name>`)
// and that the start path pins via `docker compose -p citadel-<name>`.
const citadelContainerPrefix = "citadel-"

// runningContainerNames returns the names of every running container on this
// node. It is a package var so tests can exercise the enumeration logic without
// a container runtime; production always uses listRunningContainerNames.
var runningContainerNames = listRunningContainerNames

// listRunningContainerNames runs ONE `<engineBin> ps` and returns the running
// container names. `ps` without -a lists only running containers, so no state
// filtering is needed. Any error (runtime absent, daemon down) yields nil, so
// callers degrade to "nothing enumerable here" rather than reporting a false
// stopped/absent state for services they cannot see.
func listRunningContainerNames(engineBin string) []string {
	if engineBin == "" {
		engineBin = "docker"
	}
	out, err := exec.Command(engineBin, "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// runningEmbeddedServices returns the set of embedded ServiceMap names whose
// "citadel-<name>" container is currently running. Collected ONCE per heartbeat
// and threaded through the collectors, replacing the previous one
// `docker inspect` per known engine.
//
// Only names present in services.ServiceMap are returned: a running
// citadel-claudecode or citadel-whatsapp container belongs to the module/app
// inventories (collectAppStatus), and reporting it as a service would double-
// count it.
func runningEmbeddedServices(engineBin string) map[string]bool {
	set := make(map[string]bool)
	for _, container := range runningContainerNames(engineBin) {
		name := strings.TrimPrefix(container, citadelContainerPrefix)
		if name == container {
			continue // not a citadel-managed container
		}
		if _, ok := services.ServiceMap[name]; ok {
			set[name] = true
		}
	}
	return set
}

// containerRuntimeBin resolves the container runtime binary once per collection,
// mirroring the start path (cmd/service.go). GPU/inference containers are the
// ones most likely to run under the hardened podman runtime (#348); a
// docker-only enumeration would miss them entirely.
func containerRuntimeBin() string {
	return catalog.SelectContainerRuntime().EngineBin
}

// embeddedServiceType classifies an embedded ServiceMap entry for the heartbeat.
//
// Deliberately conservative: only genuine OpenAI-compatible CHAT engines get
// ServiceTypeLLM, because that type is what makes a service a candidate for the
// gateway/chat routing surfaces. A speech (kokoro/transcribe), image
// (diffusers), or extraction service is reported as ServiceTypeOther so it shows
// up in the operator's inventory without ever being offered as a chat backend.
func embeddedServiceType(name string) string {
	switch name {
	case "tei":
		return ServiceTypeEmbedding
	case "vllm", "ollama", "llamacpp", "lmstudio", "sglang", "bonsai", "unlimited-ocr":
		return ServiceTypeLLM
	default:
		return ServiceTypeOther
	}
}

// collectRunningEmbeddedServices reports every running embedded-compose service
// that no richer collector already covered. These entries carry only what a
// container listing can honestly support (name, type, port, running) with
// Health=unknown, because this pass performs no readiness probe. That is the
// point: an unprobed service used to be absent from the inventory entirely,
// which reads as "nothing is deployed here" rather than "we did not ask".
//
// reported is the set of names already in NodeStatus.Services, so a service the
// probe-driven collectors described in detail is never overwritten by this
// coarser view.
func collectRunningEmbeddedServices(running map[string]bool, reported map[string]struct{}) []ServiceInfo {
	names := make([]string, 0, len(running))
	for name := range running {
		if _, dup := reported[name]; dup {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names) // deterministic heartbeat ordering

	out := make([]ServiceInfo, 0, len(names))
	for _, name := range names {
		out = append(out, ServiceInfo{
			Name:   name,
			Type:   embeddedServiceType(name),
			Status: ServiceStatusRunning,
			Port:   managedEngineHostPort(name),
			Health: HealthStatusUnknown,
		})
	}
	return out
}
