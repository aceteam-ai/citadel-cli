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
// KEY-ONLY env form `environment: ["OLLAMA_MAX_LOADED_MODELS"]`. Every Citadel
// compose-up path normalizes that one key through
// OllamaMaxLoadedModelsComposeEnv: an explicit manifest value wins, while an
// unset or best-effort-invalid value becomes an explicit empty string. The empty
// value is intentional: it shields Compose from an ambient shell value and from
// .env/--env-file fallback while retaining Ollama's automatic policy.
package services

import (
	"fmt"
	"strconv"
	"strings"
)

// EnvOllamaMaxLoadedModels is the env var the embedded ollama image reads to cap
// the number of models it keeps resident simultaneously. Kept as an exported
// constant so the compose template's key-only `environment:` entry and the Go
// code that injects it share one spelling.
const EnvOllamaMaxLoadedModels = "OLLAMA_MAX_LOADED_MODELS"

// OllamaMaxLoadedModelsComposeEnv returns env with exactly one effective
// OLLAMA_MAX_LOADED_MODELS entry at the end for the embedded ollama service.
// It removes every inherited duplicate before appending the manifest value, or
// an explicit empty value when the manifest field is unset. It does not mutate
// env. For every non-ollama service it returns env unchanged.
//
// A negative manifest value returns the same safe, explicit-empty environment
// plus an error. Strict callers refuse the start; best-effort callers log the
// error and use the returned environment so ambient or env-file values still
// cannot silently become node policy.
//
// Empty and explicit 0 both retain Ollama's automatic concurrency semantics.
// This was verified against official Ollama release v0.40.2, commit
// b061384d90ff455462bc32745dd0de479de717a3: envconfig.Var returns empty for an
// empty value, Uint("OLLAMA_MAX_LOADED_MODELS", 0) therefore returns 0, and
// server/sched.go treats maxRunners <= 0 as the automatic setting. Keep this
// source pin current when changing the empty-value contract.
func OllamaMaxLoadedModelsComposeEnv(env []string, service string, maxLoaded *int) ([]string, error) {
	if service != "ollama" {
		return env, nil
	}

	value := ""
	var policyErr error
	if maxLoaded != nil {
		if *maxLoaded < 0 {
			policyErr = fmt.Errorf("invalid ollama_max_loaded_models %d: must be >= 0", *maxLoaded)
		} else {
			value = strconv.Itoa(*maxLoaded)
		}
	}

	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != EnvOllamaMaxLoadedModels {
			out = append(out, entry)
		}
	}
	out = append(out, EnvOllamaMaxLoadedModels+"="+value)
	return out, policyErr
}
