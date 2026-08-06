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
// Container presence is not the whole test either. Ollama runs as a NATIVE
// systemd service on some nodes (`/usr/local/bin/ollama serve`, port 11434), so
// "no container" must not mean "not running". ResolveServiceState takes an
// injected native-serving probe and consults it only when no declared container
// is present, mirroring internal/status.managedEnginePortIfRunning (the
// heartbeat path, which already gets this right).

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

// DeclaredServices returns the set of service keys declared by a compose file.
// A nil result means the file could not be read or parsed; callers must treat
// that as "unknown" and fall back to the unfiltered view rather than concluding
// the service is stopped (a false "stopped" is a worse defect than the false
// "running" this filtering removes).
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
		Services map[string]yaml.Node `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	if len(doc.Services) == 0 {
		return nil
	}
	set := make(map[string]bool, len(doc.Services))
	for name := range doc.Services {
		set[name] = true
	}
	return set
}

// FilterPS keeps only the containers belonging to the declared services. When
// declared is empty (compose file unreadable or unparseable) it returns the
// input unchanged: fail open to the pre-existing behavior rather than reporting
// a running service as stopped.
func FilterPS(containers []PSContainer, declared map[string]bool) []PSContainer {
	if len(declared) == 0 {
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
// Order of evidence:
//  1. A declared container that is running wins, and is reported.
//  2. Otherwise, if nativeServing is non-nil and reports true, the service is
//     running natively. This is what keeps a systemd ollama from being called
//     stopped, and it deliberately outranks a non-running container: a live
//     socket is stronger evidence of serving than a stale container is of
//     stopped. Nodes accumulate exited containers (see
//     compose.RemoveLegacyProjectContainers), so an exited citadel-ollama
//     sitting next to a serving systemd ollama is a real shape. Same ordering as
//     internal/status.managedEnginePortIfRunning on the heartbeat path.
//  3. Otherwise a declared container in a non-running state is reported, with
//     its raw state kept on Container so a crash loop stays visible.
//  4. Otherwise the service is stopped. This is the case #692 got wrong.
func ResolveServiceState(psOutput []byte, declared map[string]bool, nativeServing func() bool) ServiceState {
	mine := FilterPS(ParsePS(psOutput), declared)

	var first *PSContainer
	for i := range mine {
		if mine[i].Running() {
			c := mine[i]
			return ServiceState{State: StateRunning, Running: true, Container: &c}
		}
		if first == nil {
			c := mine[i]
			first = &c
		}
	}

	if nativeServing != nil && nativeServing() {
		return ServiceState{State: StateRunning, Running: true, Native: true}
	}

	if first != nil {
		state := strings.ToLower(first.State)
		if strings.Contains(state, "exited") || strings.Contains(state, "dead") || state == "" {
			state = StateStopped
		}
		return ServiceState{State: state, Container: first}
	}
	return ServiceState{State: StateStopped}
}
