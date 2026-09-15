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

// OllamaCompose and LlamacppCompose are embedded in build-tagged files
// (compose_variants_other.go for linux/windows, compose_variants_darwin.go for
// macOS) so darwin can swap in CPU/arm64 variants that drop the nvidia GPU
// reservation Docker Desktop for macOS cannot satisfy (citadel-cli#1048). Every
// other engine's compose is embedded directly below.

//go:embed compose/vllm.yml
var VLLMCompose string

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
// direction (keep ollama/llama.cpp, drop vLLM/CUDA-only). The compose-file side
// of that promise was verified via registry-manifest checks and resolved in
// citadel-cli#1048 (follow-up to #1042):
//
//   - ollama, llamacpp: the linux compose files carry a mandatory nvidia GPU
//     reservation that fails container creation on Docker Desktop for macOS, and
//     llamacpp additionally pins a CUDA/amd64 image. Both now ship a darwin
//     CPU/arm64 compose variant (build-tagged, see compose_variants_darwin.go)
//     that drops the reservation and, for llamacpp, uses the native-arm64 CPU
//     `:server` image. They stay darwin-capable, backed by a real variant.
//
//   - transcribe, kokoro, tei, extraction: no GPU reservation, so no known
//     start failure — but their images are amd64-only (verified) or unavailable
//     to check anonymously (extraction), so on Apple Silicon they run under
//     Rosetta/QEMU emulation. Emulated start of these MKL/PyTorch-based images is
//     UNVERIFIED (Rosetta does not translate AVX/AVX2 pre-Sequoia, a common SIGILL
//     source); kept advertised because "unknown" is not "known to fail", but flag
//     it — see the Mac step in #1048 before relying on them.
//
// lmstudio was DROPPED to linuxOnlyServices: see its note there.
var darwinCapableServices = map[string]bool{
	"ollama":     true,
	"llamacpp":   true,
	"transcribe": true,
	"kokoro":     true,
	"tei":        true,
	"extraction": true,
}

// linuxOnlyServices names the embedded engines NOT advertised on darwin: the
// CUDA-only inference engines (a CUDA image and/or a mandatory NVIDIA runtime),
// plus lmstudio (see below). It exists only so
// TestServiceMapDarwinClassificationExhaustive can prove every ServiceMap key is
// classified exactly once — the runtime filter keys off darwinCapableServices
// (the allow-list) alone.
var linuxOnlyServices = map[string]bool{
	"vllm":          true,
	"sglang":        true,
	"bonsai":        true,
	"diffusers":     true,
	"unlimited-ocr": true,
	"omnivoice":     true,
	// lmstudio: dropped from darwin in citadel-cli#1048. Unlike ollama/llamacpp
	// it gets no darwin variant because there is nothing verifiable to ship — the
	// pinned image (technovangelist/lm-studio:latest) does not exist on Docker Hub
	// at all ("object not found"), so it cannot pull on any OS, and there is no
	// known arm64 image to swap in. It also carries a mandatory nvidia reservation,
	// and LM Studio on macOS is a native desktop app rather than a container. If a
	// real, pullable macOS-capable image lands, revisit.
	"lmstudio": true,
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
