package engine

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// This file holds tables and logic MOVED here from internal/status (citadel
// #685 slice 2) so this package can stop importing internal/status entirely.
// Slice 1 populated EngineSpec by calling back into internal/status
// (status.EngineDefaultModel, status.ManagedEngineHostPort, ...); that made
// internal/status -> internal/engine impossible without an import cycle
// (internal/engine -> internal/status -> internal/engine), which is exactly
// the direction slice 2 needs for internal/status's own read-path consumers
// (EngineTypeFromName, DiscoverModels, CheckServiceHealth, the
// managedProbeEngines/idleCapableEngines/embeddingProbeServices membership
// checks) to read FROM the registry. Breaking the cycle means this package
// must own these tables directly; internal/status's own package-level vars
// of the same name are now derived FROM these at init instead (see
// internal/status/engines.go and hotswap.go), reversing slice 1's direction.
//
// Every value below is copied verbatim from the internal/status tables it
// replaces -- moved, not changed, same as slice 1's literal copies of
// internal/worker's/internal/jobs's tables in registry.go.

// ---------------------------------------------------------------------------
// Ordered engine-name lists.
//
// Order is preserved EXACTLY from the pre-migration internal/status tables --
// deliberately NOT re-derived from Registry.All(), which sorts
// alphabetically. Callers such as status.DiscoverLocalEngines iterate these
// lists directly, so silently reordering them would be an observable
// behavior change (which engine answers an ambiguous/first-match query),
// not a cosmetic one.
// ---------------------------------------------------------------------------

// managedProbeEngineNames lists the managed serving engines the heartbeat's
// model/health probe iterates. Moved from internal/status's
// managedProbeEngines (internal/status/engines.go).
var managedProbeEngineNames = []string{"vllm", "ollama", "llamacpp", "bonsai", "unlimited-ocr", "sglang"}

// idleCapableEngineNames lists the engines with a reliable SCRAPED
// idle/request signal (currently only vLLM's Prometheus counters). Moved
// from internal/status's idleCapableEngines.
var idleCapableEngineNames = []string{"vllm"}

// embeddingCapableEngineNames lists the OpenAI-compatible embedding services
// probed on the heartbeat. Moved from internal/status's
// embeddingProbeServices.
var embeddingCapableEngineNames = []string{"tei"}

// ManagedProbeEngineNames returns a copy of managedProbeEngineNames.
func ManagedProbeEngineNames() []string {
	out := make([]string, len(managedProbeEngineNames))
	copy(out, managedProbeEngineNames)
	return out
}

// IdleCapableEngineNames returns a copy of idleCapableEngineNames.
func IdleCapableEngineNames() []string {
	out := make([]string, len(idleCapableEngineNames))
	copy(out, idleCapableEngineNames)
	return out
}

// EmbeddingCapableEngineNames returns a copy of embeddingCapableEngineNames.
func EmbeddingCapableEngineNames() []string {
	out := make([]string, len(embeddingCapableEngineNames))
	copy(out, embeddingCapableEngineNames)
	return out
}

// ---------------------------------------------------------------------------
// Model env / default / VRAM tables. Moved verbatim from
// internal/status/hotswap.go's engineModelEnvVars/engineDefaultModel/
// engineVRAMEstimateMB.
// ---------------------------------------------------------------------------

var modelEnvVarsByEngine = map[string][]string{
	"vllm":          {"VLLM_MODEL"},
	"unlimited-ocr": {"OCR_SERVED_NAME", "OCR_MODEL"},
	"bonsai":        {"BONSAI_MODEL"},
	"llamacpp":      {"LLAMACPP_MODEL"},
}

var defaultModelByEngine = map[string]string{
	"unlimited-ocr": "baidu/Unlimited-OCR",
	"bonsai":        "Bonsai-27B-Q1_0.gguf",
}

var vramEstimateMBByEngine = map[string]int{
	"vllm":          22000,
	"sglang":        22000,
	"unlimited-ocr": 20000,
	"bonsai":        22000,
	"llamacpp":      8000,
	"ollama":        8000,
}

// ---------------------------------------------------------------------------
// Host port resolution. Moved from internal/status/engines.go's
// managedEngineHostPort/firstComposeHostPort/composePortRe.
//
// HostPortForName is deliberately a GENERAL resolver over any embedded
// service name, not scoped to Registry.Lookup: callers such as
// status.collectRunningEmbeddedServices resolve a host port for every
// running "citadel-<name>" container, including non-chat, non-ServiceMap
// services (gotenberg, nvr, meeting, storage, claudecode, hermes) that have
// a citadel-owned host port via services.ManagedServiceHostPort but no
// services.ServiceMap/Registry entry at all. Narrowing this to
// Registry.Lookup would silently return 0 for every one of those.
// buildSpec below only ever calls it with services.ServiceMap keys (the
// registry's own domain), which is a strict subset of what this function
// supports.
// ---------------------------------------------------------------------------

// composePortRe matches the host side of a compose short-form port mapping,
// e.g. "8100:8000" or "127.0.0.1:8100:8000" -> host port 8100.
var composePortRe = regexp.MustCompile(`(?:\d+\.\d+\.\d+\.\d+:)?(\d+):\d+`)

// HostPortForName resolves the published host port for a managed embedded
// service by name: the citadel-owned registry (services.ManagedServiceHostPort)
// first, falling back to parsing the first port mapping of the service's
// embedded compose file (services.ServiceMap) for a service with no
// citadel-owned host port entry. Returns 0 when neither yields a port.
func HostPortForName(name string) int {
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

// ---------------------------------------------------------------------------
// Engine-type-from-name (substring matching). Moved+merged from the two
// independently-maintained copies design doc §1c found:
// internal/status.EngineTypeFromName and internal/mesh's own duplicate
// (internal/mesh/discovery.go). Both now delegate here.
// ---------------------------------------------------------------------------

// engineNameAliases lists EXTRA substring spellings for an engine's own name
// that TypeFromName must also recognize, beyond the bare name itself --
// llamacpp's alternate on-disk/container-name spellings. Not derivable from
// EngineSpec (no such field exists), so this stays a small local table.
var engineNameAliases = map[string][]string{
	"llamacpp": {"llama.cpp", "llama-cpp"},
}

// TypeFromName maps a service/app name to a model-discovery engine type
// ("vllm", "ollama", "llamacpp", "bonsai", "unlimited-ocr", "sglang"), or ""
// when the name is not a known managed-probe engine.
//
// Matching is substring (strings.Contains), case-insensitive, checked
// against managedProbeEngineNames' own order plus engineNameAliases. That
// order is NOT load-bearing for correctness here (unlike the ordered-list
// exports above): none of today's patterns are substrings of one another
// (verified by inspection and pinned by TestTypeFromName), so it is kept
// only for continuity with the pre-migration switch statement's historical
// ordering comment.
func TypeFromName(name string) string {
	n := strings.ToLower(name)
	for _, eng := range managedProbeEngineNames {
		if strings.Contains(n, eng) {
			return eng
		}
		for _, alias := range engineNameAliases[eng] {
			if strings.Contains(n, alias) {
				return eng
			}
		}
	}
	return ""
}
