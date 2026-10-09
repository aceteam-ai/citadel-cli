// services/ollama_policy.go
//
// A supported, restart-surviving policy surface for the embedded ollama service
// (aceteam-ai/citadel-cli#1209). Ollama is one long-running engine that manages
// model residency internally; OLLAMA_MAX_LOADED_MODELS caps how many models it
// keeps resident at once. On a memory-tight node (e.g. 8 GB) an operator needs a
// declarative "one model resident" policy that survives `citadel work` and
// service restarts, which a hand-run `docker run -e ...` does not.
//
// This mirrors the per-service `bind:` escape hatch (services/bind.go) EXACTLY in
// shape -- a dedicated typed manifest field injected as an env var at
// `docker compose up`, never an arbitrary env-passthrough map -- but is kept
// deliberately separate from the bind machinery: TestServiceMapBindSweep and the
// aceteam-ai/citadel-cli#1030 loop-guard key off serviceBindEnv/BindEnv being
// bind-only, so this must not widen them.
//
// Mechanism: services/compose/ollama.yml (and its darwin variant) declare the
// KEY-ONLY env form `environment: ["OLLAMA_MAX_LOADED_MODELS"]`, so when the var
// is ABSENT from the compose process environment compose passes nothing through
// (the container env is byte-equivalent to before this field existed). citadel
// injects OLLAMA_MAX_LOADED_MODELS=<n> into that process environment ONLY when
// the manifest field is set (OllamaMaxLoadedModelsEnv returns inject=false
// otherwise), exactly how BindEnv injects CITADEL_<SVC>_BIND only on an explicit
// `bind:` value.
package services

import "fmt"

// EnvOllamaMaxLoadedModels is the env var the embedded ollama image reads to cap
// the number of models it keeps resident simultaneously. Kept as an exported
// constant so the compose template's key-only `environment:` entry and the Go
// code that injects it share one spelling.
const EnvOllamaMaxLoadedModels = "OLLAMA_MAX_LOADED_MODELS"

// OllamaMaxLoadedModelsEnv returns the "OLLAMA_MAX_LOADED_MODELS=<n>" entry to
// inject for the embedded ollama service given its manifest
// `ollama_max_loaded_models:` value, and whether to inject it. Contract (mirrors
// BindEnv):
//
//   - service != "ollama"  -> ("", false, nil)  // env var only means anything
//     to the ollama image; never injected for another engine.
//   - maxLoaded == nil      -> ("", false, nil)  // unset: inject nothing, the
//     compose key-only `environment:` entry then passes nothing through.
//   - maxLoaded < 0         -> ("", false, error) // nonsensical; refuse loudly
//     (the ResolveBindAddr "typo refuses rather than guesses" posture) rather
//     than feed a negative to the engine.
//   - maxLoaded >= 0         -> ("OLLAMA_MAX_LOADED_MODELS=<n>", true, nil)
//
// 0 is a legitimate explicit value (ollama treats it as "auto/default"), which is
// why the field is a *int: nil (unset) must be distinguishable from an explicit
// 0, and only the former means "inject nothing".
func OllamaMaxLoadedModelsEnv(service string, maxLoaded *int) (string, bool, error) {
	if service != "ollama" || maxLoaded == nil {
		return "", false, nil
	}
	if *maxLoaded < 0 {
		return "", false, fmt.Errorf("invalid ollama_max_loaded_models %d: must be >= 0", *maxLoaded)
	}
	return fmt.Sprintf("%s=%d", EnvOllamaMaxLoadedModels, *maxLoaded), true, nil
}
