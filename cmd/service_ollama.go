// cmd/service_ollama.go
//
// cmd-side glue for the aceteam-ai/citadel-cli#1209 ollama residency policy
// (OLLAMA_MAX_LOADED_MODELS). The pure resolution lives in
// services/ollama_policy.go; this file turns the manifest
// `ollama_max_loaded_models:` field into the env entry the compose key-only
// `environment: [OLLAMA_MAX_LOADED_MODELS]` passthrough consumes, mirroring
// cmd/service_bind.go's role for the #1023 bind hatch.
package cmd

import (
	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// ollamaMaxLoadedModelsEntriesStrict returns the OLLAMA_MAX_LOADED_MODELS=<n>
// env entry (0 or 1) to inject at `docker compose up` for serviceName given its
// manifest value, surfacing an invalid value as an error. Used by the
// operator-facing start path (startService), which refuses loudly so the
// operator can fix the manifest.
func ollamaMaxLoadedModelsEntriesStrict(serviceName string, maxLoaded *int) ([]string, error) {
	entry, inject, err := svcports.OllamaMaxLoadedModelsEnv(serviceName, maxLoaded)
	if err != nil {
		return nil, err
	}
	if !inject {
		return nil, nil
	}
	return []string{entry}, nil
}

// ollamaMaxLoadedModelsEntries is the best-effort flavor for paths where a start
// must not fail on a bad value (the boot-time port-drift recreate): an invalid
// value is logged and dropped so the engine default applies, matching
// composeEnvForService's bind handling.
func ollamaMaxLoadedModelsEntries(serviceName string, maxLoaded *int) []string {
	entries, err := ollamaMaxLoadedModelsEntriesStrict(serviceName, maxLoaded)
	if err != nil {
		Log("ollama policy: %s: %v; using engine default", serviceName, err)
		return nil
	}
	return entries
}
