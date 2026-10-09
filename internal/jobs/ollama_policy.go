// internal/jobs/ollama_policy.go
//
// jobs-side glue for the aceteam-ai/citadel-cli#1209 ollama residency policy
// (OLLAMA_MAX_LOADED_MODELS), mirroring internal/jobs/bind.go for the #1023 bind
// hatch. The APPLY_DEVICE_CONFIG compose-up (config_handler.go) does not route
// through ServiceHandler.serviceStart -- the primary injection site -- so it
// reads the manifest `ollama_max_loaded_models:` field directly from the file
// and threads it into the compose process env. The pure resolution lives in
// services/ollama_policy.go.
package jobs

import (
	"os"

	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// ollamaMaxLoadedModelsFromFile reads the manifest at manifestPath (best-effort)
// and returns the #1209 `ollama_max_loaded_models:` value for serviceName, or nil
// when the file is unreadable/unparseable, the service is absent, or the field is
// unset. The caller normalizes nil to explicit empty so Compose cannot fall
// back to a hostile shell or env-file value.
func ollamaMaxLoadedModelsFromFile(manifestPath, serviceName string) *int {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	var doc struct {
		Services []struct {
			Name                  string `yaml:"name"`
			OllamaMaxLoadedModels *int   `yaml:"ollama_max_loaded_models"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	for _, s := range doc.Services {
		if s.Name == serviceName {
			return s.OllamaMaxLoadedModels
		}
	}
	return nil
}

// composeEnvForAppliedService is the hermetic core of APPLY_DEVICE_CONFIG's
// compose-process environment policy. It deliberately accepts the manifest
// path so tests can use a private fixture rather than the live node config.
func composeEnvForAppliedService(env []string, manifestPath, serviceName string) ([]string, error) {
	return services.OllamaMaxLoadedModelsComposeEnv(
		env, serviceName, ollamaMaxLoadedModelsFromFile(manifestPath, serviceName))
}
