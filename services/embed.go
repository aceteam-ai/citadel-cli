// services/embed.go
package services

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

//go:embed compose/ollama.yml
var OllamaCompose string

//go:embed compose/vllm.yml
var VLLMCompose string

//go:embed compose/llamacpp.yml
var LlamacppCompose string

//go:embed compose/lmstudio.yml
var LMStudioCompose string

//go:embed compose/sglang.yml
var SGLangCompose string

//go:embed compose/extraction.yml
var ExtractionCompose string

//go:embed compose/transcribe.yml
var TranscribeCompose string

//go:embed compose/diffusers.yml
var DiffusersCompose string

//go:embed compose/bonsai.yml
var BonsaiCompose string

// BonsaiDockerfile is the build-context Dockerfile for the bonsai service. It is
// materialized to <config>/services/bonsai/Dockerfile (see WriteAuxFiles) so the
// compose `build.context: ./bonsai` resolves on the node.
//
//go:embed compose/bonsai/Dockerfile
var BonsaiDockerfile string

// ServiceMap provides a lookup for pre-packaged service compose files.
var ServiceMap = map[string]string{
	"ollama":     OllamaCompose,
	"vllm":       VLLMCompose,
	"llamacpp":   LlamacppCompose,
	"lmstudio":   LMStudioCompose,
	"sglang":     SGLangCompose,
	"extraction": ExtractionCompose,
	"transcribe": TranscribeCompose,
	"diffusers":  DiffusersCompose,
	"bonsai":     BonsaiCompose,
}

// ServiceAuxFiles maps a service name to auxiliary build-context files
// (path relative to the node's services/ dir -> content) that must be
// materialized alongside the service's <name>.yml for it to start.
//
// bonsai is the first embedded service that BUILDS its image from a Dockerfile
// (every other entry uses a prebuilt image:), so its compose
// `build.context: ./bonsai` needs services/bonsai/Dockerfile on disk. Without
// this the .yml materializes fine but `docker compose build` on the node fails
// with "Dockerfile not found".
var ServiceAuxFiles = map[string]map[string]string{
	"bonsai": {
		filepath.Join("bonsai", "Dockerfile"): BonsaiDockerfile,
	},
}

// WriteAuxFiles materializes any build-context files a service needs into
// servicesDir (the node's <config>/services directory). It is idempotent and a
// no-op for services with no aux files. Callers invoke it wherever they
// materialize a service's <name>.yml so a build-based service is startable.
func WriteAuxFiles(servicesDir, name string) error {
	aux, ok := ServiceAuxFiles[name]
	if !ok {
		return nil
	}
	for rel, content := range aux {
		dest := filepath.Join(servicesDir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return fmt.Errorf("create build-context dir for %s: %w", name, err)
		}
		if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
			return fmt.Errorf("write build-context file %s: %w", rel, err)
		}
	}
	return nil
}

// GetAvailableServices returns a sorted list of service names.
func GetAvailableServices() []string {
	keys := make([]string, 0, len(ServiceMap))
	for k := range ServiceMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
