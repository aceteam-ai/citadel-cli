// services/embed.go
package services

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

//go:embed compose/kokoro.yml
var KokoroCompose string

//go:embed compose/tei.yml
var TEICompose string

//go:embed compose/unlimited-ocr.yml
var UnlimitedOCRCompose string

//go:embed compose/omnivoice.yml
var OmniVoiceCompose string

// BonsaiDockerfile is the build-context Dockerfile for the bonsai service. It is
// materialized to <config>/services/bonsai/Dockerfile (see WriteAuxFiles) so the
// compose `build.context: ./bonsai` resolves on the node.
//
//go:embed compose/bonsai/Dockerfile
var BonsaiDockerfile string

// ServiceMap provides a lookup for pre-packaged service compose files.
var ServiceMap = map[string]string{
	"ollama":        OllamaCompose,
	"vllm":          VLLMCompose,
	"llamacpp":      LlamacppCompose,
	"lmstudio":      LMStudioCompose,
	"sglang":        SGLangCompose,
	"extraction":    ExtractionCompose,
	"transcribe":    TranscribeCompose,
	"diffusers":     DiffusersCompose,
	"bonsai":        BonsaiCompose,
	"kokoro":        KokoroCompose,
	"tei":           TEICompose,
	"unlimited-ocr": UnlimitedOCRCompose,
	"omnivoice":     OmniVoiceCompose,
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

// darwinCapableServices is the ALLOW-LIST of embedded engines that can run on
// macOS (darwin). It is deliberately an allow-list, not a deny-list
// (citadel-cli#1042): a newly added engine is hidden on darwin by default until
// it is explicitly classified, so a CUDA-only engine can never be advertised on
// a Mac by omission. TestServiceMapDarwinClassificationExhaustive forces every
// ServiceMap key to be classified as darwin-capable or linux-only.
//
// This reflects engine SOFTWARE capability on macOS, per the issue's explicit
// direction (keep ollama/llama.cpp, drop vLLM/CUDA-only). CAVEAT: the LLM engine
// compose files (ollama, llamacpp, lmstudio) currently declare an NVIDIA GPU
// reservation and llamacpp pins a CUDA image tag; whether they start unmodified
// under Docker Desktop on Apple Silicon is unverified and tracked as a follow-up
// (needs a real Mac to confirm). This filter's job is to stop advertising the
// unambiguously CUDA-only engines a Mac can never run.
var darwinCapableServices = map[string]bool{
	"ollama":     true,
	"llamacpp":   true,
	"lmstudio":   true,
	"transcribe": true,
	"kokoro":     true,
	"tei":        true,
	"extraction": true,
}

// linuxOnlyServices names the embedded engines that are CUDA-only (a CUDA image
// and/or a mandatory NVIDIA runtime) and therefore never advertised on darwin.
// It exists only so TestServiceMapDarwinClassificationExhaustive can prove every
// ServiceMap key is classified exactly once — the runtime filter keys off
// darwinCapableServices (the allow-list) alone.
var linuxOnlyServices = map[string]bool{
	"vllm":          true,
	"sglang":        true,
	"bonsai":        true,
	"diffusers":     true,
	"unlimited-ocr": true,
	"omnivoice":     true,
}

// availableServicesFor returns the sorted service names available on the given
// GOOS. On darwin, engines not in darwinCapableServices are filtered out; every
// other OS gets the full ServiceMap. It is split from GetAvailableServices so
// the GOOS filter is unit-testable off-host (this build runs on linux CI).
func availableServicesFor(goos string) []string {
	keys := make([]string, 0, len(ServiceMap))
	for k := range ServiceMap {
		if goos == "darwin" && !darwinCapableServices[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// GetAvailableServices returns a sorted list of service names this build can
// deploy on the current OS. On macOS the list excludes CUDA-only engines so a
// Mac never advertises an engine it cannot start (citadel-cli#1042).
func GetAvailableServices() []string {
	return availableServicesFor(runtime.GOOS)
}
