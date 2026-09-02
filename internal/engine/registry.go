package engine

import (
	"sort"
	"time"

	"github.com/aceteam-ai/citadel-cli/services"
)

// Registry looks up an Engine by name. Populated once, at package init, by
// buildRegistry() below -- a translation of the existing tables, not a new
// source of truth.
type Registry struct {
	engines map[string]Engine
}

// Lookup returns the Engine for name and whether it was found.
func (r *Registry) Lookup(name string) (Engine, bool) {
	e, ok := r.engines[name]
	return e, ok
}

// All returns every registered Engine, sorted by name for deterministic
// iteration (tests and any future printer/lister want a stable order, and
// nothing here needs map iteration's randomness).
func (r *Registry) All() []Engine {
	names := make([]string, 0, len(r.engines))
	for name := range r.engines {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Engine, 0, len(names))
	for _, name := range names {
		out = append(out, r.engines[name])
	}
	return out
}

var defaultRegistry = buildRegistry()

// Default returns the package's Registry, built once at init from the
// existing per-engine tables. See buildRegistry for exactly which table feeds
// which field.
func Default() *Registry {
	return defaultRegistry
}

// readyPathByEngine is a literal copy of internal/worker's engineReadyPath
// (internal/worker/llm_readiness.go). Copied, not imported, per this
// package's leaf constraint (see types.go's package doc) -- verified against
// the real table by TestRegistryEquivalence via the exported
// worker.EngineReadyPath accessor.
var readyPathByEngine = map[string]string{
	"vllm":          "/health",
	"sglang":        "/health",
	"unlimited-ocr": "/v1/models",
	"llamacpp":      "/v1/models",
	"bonsai":        "/v1/models",
	"ollama":        "/api/tags",
}

// loadEstimateByEngine is a literal copy of internal/worker's
// defaultLoadEstimate (internal/worker/swap.go). That function is a SWITCH
// WITH A DEFAULT CASE -- a total function with no "absent" state, unlike
// e.g. engineReadyPath (a map lookup, genuinely absent for most engines) --
// so a faithful translation must give every ServiceMap engine an entry,
// including the six that are not llm_inference backends at all and only ever
// hit via the default branch (60s). Recording 0 for those instead would be a
// fabricated divergence, not a translation: TestRegistryEquivalence asserts
// spec.LoadEstimate == worker.DefaultLoadEstimate(name) unconditionally, for
// all twelve, and this table exists to satisfy that for the six with no
// named case the same way the real switch's fallthrough does.
var loadEstimateByEngine = map[string]time.Duration{
	"bonsai":        3 * time.Minute,
	"vllm":          90 * time.Second,
	"sglang":        90 * time.Second,
	"unlimited-ocr": 90 * time.Second,
	// The remaining six ServiceMap engines (llamacpp, ollama, and the four
	// non-chat engines below) have no named case in the real switch, so they
	// fall to its "default: 60s" branch -- translated verbatim, not applied
	// selectively.
	"llamacpp":   60 * time.Second,
	"ollama":     60 * time.Second,
	"diffusers":  60 * time.Second,
	"extraction": 60 * time.Second,
	"transcribe": 60 * time.Second,
	"kokoro":     60 * time.Second,
	"tei":        60 * time.Second,
	"lmstudio":   60 * time.Second,
}

// selfProvisioningEngines is a literal copy of internal/jobs's
// selfProvisioningEngines (internal/jobs/model_cache_pull.go) -- engines
// whose compose file owns its weights, so MODEL_CACHE_PULL is a no-op for
// them. Copied, not imported, per this package's leaf constraint; verified
// against the real table by TestRegistryEquivalence via the exported
// jobs.IsSelfProvisioningEngine accessor.
var selfProvisioningEngines = map[string]bool{
	"tei":           true,
	"diffusers":     true,
	"kokoro":        true,
	"transcribe":    true,
	"unlimited-ocr": true,
	"extraction":    true,
}

// composeKindEngines lists the services.ServiceMap engines that are
// EngineKind=NativeProcess rather than ComposeService. Only ollama today
// (design doc §2's coverage table: "ollama | NativeProcess (fixed port)").
var nativeProcessEngines = map[string]bool{
	"ollama": true,
}

// dialectByEngine assigns each engine's PRIMARY request dialect per design
// doc §1e/§2. This is new synthesis, not a translation of an existing table
// (none exists) -- see RequestDialect's doc comment in types.go for what the
// zero value means and why lmstudio/tei/diffusers/extraction/transcribe/
// kokoro get it.
//
// vllm is deliberately assigned OpenAIChat even though
// internal/worker/llm_inference.go's executeVLLM dynamically picks BETWEEN
// OpenAIChat (payload.Messages set) and OpenAICompletions (prompt-only) --
// EngineSpec has one Dialect field, and design doc §2's own coverage table
// lists vllm as supporting both. Chat is the primary/gateway-routed path
// today (see internal/gateway/chat_route.go); the completions fallback is
// not lost, just not representable as a second value in this slice's single-
// Dialect shape. A later slice (Phase B, three dialect implementations) is
// where this gets revisited if a real caller needs both.
var dialectByEngine = map[string]RequestDialect{
	"vllm":          OpenAIChat,
	"sglang":        CompletionsOnly,
	"ollama":        OllamaNative,
	"llamacpp":      OpenAIChat,
	"bonsai":        OpenAIChat,
	"unlimited-ocr": OpenAIChat,
}

// buildRegistry translates the existing per-engine tables into one EngineSpec
// per services.ServiceMap engine. This is Phase A from design-engine-adapter.md
// §3: a translation layer, not new logic. Every field either calls an
// existing table directly (services/internal/status, both importable here)
// or reads one of the literal copies above (for the two tables that live in
// packages this leaf must not import -- see types.go's package doc).
//
// Deliberately does NOT fix any gap it encounters (see llamacpp's absent
// DefaultModel, sglang's absent ModelEnvVar, etc. below) -- moved, not
// changed, so TestRegistryEquivalence can assert byte-for-byte parity with
// the tables it mirrors.
func buildRegistry() *Registry {
	engines := make(map[string]Engine, len(services.ServiceMap))
	for name := range services.ServiceMap {
		engines[name] = specEngine{spec: buildSpec(name)}
	}
	return &Registry{engines: engines}
}

func buildSpec(name string) EngineSpec {
	kind := ComposeService
	if nativeProcessEngines[name] {
		kind = NativeProcess
	}

	hostPortEnvVar, _ := services.HostPortEnvVarName(name)

	var cacheDir string
	var cacheFamily services.CacheFamily
	if c, ok := services.EngineCacheDirs[name]; ok {
		cacheDir = c.Dir
		cacheFamily = c.Family
	}

	var defaultModel *string
	if v, ok := defaultModelByEngine[name]; ok {
		defaultModel = &v
	}

	return EngineSpec{
		Name:             name,
		Kind:             kind,
		HostPort:         HostPortForName(name),
		HostPortEnvVar:   hostPortEnvVar,
		CacheDir:         cacheDir,
		CacheFamily:      cacheFamily,
		Dialect:          dialectByEngine[name],
		ReadyPath:        readyPathByEngine[name],
		LoadEstimate:     loadEstimateByEngine[name],
		VRAMEstimateMB:   vramEstimateMBByEngine[name],
		ModelEnvVar:      copyStringSlice(modelEnvVarsByEngine[name]),
		DefaultModel:     defaultModel,
		IdleCapable:      stringInSlice(idleCapableEngineNames, name),
		EmbeddingCapable: stringInSlice(embeddingCapableEngineNames, name),
		ManagedProbe:     stringInSlice(managedProbeEngineNames, name),
		MetricsPort:      services.InferenceMetricsPorts()[name],
		SelfProvisioning: selfProvisioningEngines[name],
	}
}

// copyStringSlice returns a fresh copy of vars, or nil when vars is nil --
// mirroring the nil-vs-empty distinction internal/status.EngineModelEnvVars
// used to preserve for its callers (a nil ModelEnvVar means "no entry in the
// table", not "an entry with zero variables").
func copyStringSlice(vars []string) []string {
	if vars == nil {
		return nil
	}
	out := make([]string, len(vars))
	copy(out, vars)
	return out
}

func stringInSlice(list []string, name string) bool {
	for _, v := range list {
		if v == name {
			return true
		}
	}
	return false
}
