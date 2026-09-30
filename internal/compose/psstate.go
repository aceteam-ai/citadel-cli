package compose

import (
	"encoding/json"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file resolves the single question every operator surface asks about a
// managed service: is it actually running?
//
// The naive answer, shell `docker compose -f <file> ps --format json` and read
// the first container, is wrong on a citadel node, and citadel-cli#692 is what
// that wrongness looks like: all 11 declared services report running while three
// of them have no container at all.
//
// Why: citadel deliberately passes NO `-p` to compose (#528, pinned by
// TestUpdateManifestNoProjectFlag). Every service compose file lives in the same
// directory and none declares a top level `name:`, so they ALL share one default
// compose project, the directory basename ("services"). `ps` is therefore
// project-scoped, not file-scoped: `-f vllm.yml ps` returns bonsai, gotenberg,
// kokoro and friends. Verified on a live node.
//
// The fix is NOT to add `-p citadel-<name>`, which is what #692 suggests: that
// would reintroduce exactly the project-name mismatch #528 removed, and status
// would then report every service stopped because the containers actually live
// in the shared project. Instead, filter the project-wide `ps` output down to
// the services the compose file itself declares. That works for both naming
// conventions in use: the pinned `container_name: citadel-<name>` most services
// use, and compose's own `<project>-<service>-<n>` (whatsapp-bridge's `bridge`
// and `db`, which a `citadel-<name>` container lookup would miss entirely).
//
// Every non-profiled service declared by one compose file is part of that
// managed service. A multi-container module is running only when every one of
// those components is running; one surviving sidecar must not mask a missing or
// exited sibling.
//
// Container presence is not the whole test either. Ollama runs as a NATIVE
// systemd service on some nodes (`/usr/local/bin/ollama serve`, port 11434), so
// "no container" must not mean "not running". ResolveServiceState takes an
// injected native-serving probe and consults it for a single-service compose
// file only, mirroring internal/status.managedEnginePortIfRunning (the
// heartbeat path, which already gets this right) without allowing one native
// process to satisfy a multi-container module.

// PSContainer is the subset of a `docker compose ps --format json` record the
// operator surfaces read.
type PSContainer struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Image   string `json:"Image"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Ports   string `json:"Ports"`
}

// Running reports whether the container's state reads as up.
func (c PSContainer) Running() bool {
	state := strings.ToLower(c.State)
	return strings.Contains(state, "running") || strings.Contains(state, "up")
}

// ServiceState is the resolved run state of one manifest service.
type ServiceState struct {
	// State is the normalized state string: "running", "stopped", or the raw
	// docker state ("restarting", "paused", ...) when it is neither.
	State string
	// Running is true when the service is serving, whether by container or as a
	// native process.
	Running bool
	// Native is true when no declared container exists but the injected native
	// probe reported the service serving (e.g. ollama under systemd).
	Native bool
	// Container is the container the state was read from, or nil when the
	// service is native or absent.
	Container *PSContainer
}

const (
	// StateRunning and StateStopped are the two normalized states.
	StateRunning = "running"
	StateStopped = "stopped"
)

// ParsePS decodes `docker compose ps --format json` output. Compose emits one
// JSON object per line on some versions and a single JSON array on others, so
// both forms are accepted; malformed records are skipped rather than failing the
// whole read.
func ParsePS(output []byte) []PSContainer {
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []PSContainer
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			return arr
		}
		return nil
	}
	var out []PSContainer
	dec := json.NewDecoder(strings.NewReader(trimmed))
	for dec.More() {
		var c PSContainer
		if err := dec.Decode(&c); err != nil {
			break
		}
		out = append(out, c)
	}
	return out
}

// DeclaredServices returns the set of service keys that Compose starts by
// default. Services guarded by `profiles:` are excluded because citadel does
// not enable profiles on its compose-up/status paths. A nil result means the
// file could not be read or parsed; callers must treat that as "unknown" and
// fall back to the unfiltered view rather than concluding the service is
// stopped (a false "stopped" is a worse defect than the false "running" this
// filtering removes).
func DeclaredServices(composePath string) map[string]bool {
	data, err := os.ReadFile(composePath)
	if err != nil {
		return nil
	}
	return DeclaredServicesFromYAML(data)
}

// DeclaredServicesFromYAML is the pure form of DeclaredServices.
func DeclaredServicesFromYAML(data []byte) map[string]bool {
	var doc struct {
		Services map[string]struct {
			Profiles []string `yaml:"profiles"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	if len(doc.Services) == 0 {
		return nil
	}
	set := make(map[string]bool, len(doc.Services))
	for name, service := range doc.Services {
		if len(service.Profiles) > 0 {
			continue
		}
		set[name] = true
	}
	return set
}

// FilterPS keeps only the containers belonging to the declared services. A nil
// set means the compose file was unreadable or unparseable, so it returns the
// input unchanged: fail open to the pre-existing behavior rather than reporting
// a running service as stopped. A known empty set (for example a file containing
// only profile-gated services) correctly matches no containers.
func FilterPS(containers []PSContainer, declared map[string]bool) []PSContainer {
	if declared == nil {
		return containers
	}
	out := make([]PSContainer, 0, len(containers))
	for _, c := range containers {
		if declared[c.Service] {
			out = append(out, c)
		}
	}
	return out
}

// ResolveServiceState decides whether a manifest service is running, given the
// project-wide `docker compose ps --format json` output, the set of services its
// compose file declares, and an optional native-serving probe.
//
// For a known compose file, every non-profiled declared component must have a
// running container. A single-service file may instead be satisfied by the
// native-serving probe; this keeps a systemd ollama healthy even beside a stale
// exited container. A multi-service file may not use that fallback because one
// native socket cannot prove its other components are alive.
//
// A nil declared set means the file could not be parsed. That deliberately
// retains the historical fail-open behavior: select any running project
// container rather than introducing a false stopped result.
func ResolveServiceState(psOutput []byte, declared map[string]bool, nativeServing func() bool) ServiceState {
	containers := ParsePS(psOutput)
	if declared == nil {
		return resolveAnyServiceState(containers, nativeServing)
	}

	var representative *PSContainer
	for serviceName := range declared {
		var first, running *PSContainer
		for i := range containers {
			if containers[i].Service != serviceName {
				continue
			}
			container := containers[i]
			if first == nil {
				first = &container
			}
			if container.Running() {
				running = &container
				break
			}
		}
		if running != nil {
			if representative == nil {
				representative = running
			}
			continue
		}

		// Native serving is an alternative only for a compose file representing
		// one service. It deliberately outranks that service's stale container.
		if len(declared) == 1 && nativeServing != nil && nativeServing() {
			return ServiceState{State: StateRunning, Running: true, Native: true}
		}
		return stoppedServiceState(first)
	}

	if representative != nil {
		return ServiceState{State: StateRunning, Running: true, Container: representative}
	}
	return ServiceState{State: StateStopped}
}

func resolveAnyServiceState(containers []PSContainer, nativeServing func() bool) ServiceState {
	var first *PSContainer
	for i := range containers {
		container := containers[i]
		if container.Running() {
			return ServiceState{State: StateRunning, Running: true, Container: &container}
		}
		if first == nil {
			first = &container
		}
	}
	if nativeServing != nil && nativeServing() {
		return ServiceState{State: StateRunning, Running: true, Native: true}
	}
	return stoppedServiceState(first)
}

func stoppedServiceState(container *PSContainer) ServiceState {
	if container == nil {
		return ServiceState{State: StateStopped}
	}
	state := strings.ToLower(container.State)
	if strings.Contains(state, "exited") || strings.Contains(state, "dead") || state == "" {
		state = StateStopped
	}
	return ServiceState{State: state, Container: container}
}
