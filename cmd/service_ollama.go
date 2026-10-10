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
	"fmt"

	svcports "github.com/aceteam-ai/citadel-cli/services"
)

// composeEnvForStrictServiceStart constructs startService's complete compose
// process environment before any runtime selection, inspection, adoption, or
// stale-container removal can occur. A negative manifest policy returns an
// error here so startService remains side-effect-free on invalid input.
func composeEnvForStrictServiceStart(serviceName string, service Service, bindEnv map[string]string) ([]string, error) {
	// Validate against an empty slice first: composeEnv resolves/creates the
	// workspace, so it must not run for a policy that will be refused.
	if _, err := svcports.OllamaMaxLoadedModelsComposeEnv(nil, serviceName, service.OllamaMaxLoadedModels); err != nil {
		return nil, err
	}
	env := append(composeEnv(), bindEnvEntries(bindEnv)...)
	env, err := svcports.OllamaMaxLoadedModelsComposeEnv(env, serviceName, service.OllamaMaxLoadedModels)
	if err != nil {
		return nil, err
	}
	// aceteam-ai/citadel-cli#1269: GPU-aware tei image tag / CPU thread opt-in
	// (tei only; nil for every other service).
	return append(env, teiComposeEnvEntries(serviceName, service.Threads)...), nil
}

// warnIfOllamaPolicyIgnoredNative prints a warning when an ollama service carries
// an #1209 ollama_max_loaded_models policy but resolved to the NATIVE engine (a
// host-managed ollama, or an explicit type: native) -- there is no compose to
// inject OLLAMA_MAX_LOADED_MODELS into, so the policy is a silent no-op otherwise.
// Both boot-time start paths (runAllServices, startManagedServices) call this
// before startNativeService so an operator sees why their residency cap did not
// take effect (pin `type: docker` to apply it), rather than silent unlimited
// residency on a memory-tight node.
func warnIfOllamaPolicyIgnoredNative(service Service) {
	if service.Name == "ollama" && service.OllamaMaxLoadedModels != nil {
		fmt.Printf("   ⚠️  ollama_max_loaded_models is set but %s resolved to native (no compose to inject into); pin `type: docker` in citadel.yaml to apply OLLAMA_MAX_LOADED_MODELS\n", service.Name)
	}
}
