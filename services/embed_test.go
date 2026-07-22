package services

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposeFilesAreValidYAML verifies that all embedded compose files
// parse as valid YAML. This catches syntax errors at test time rather
// than at runtime when a user runs `citadel run <service>`.
func TestComposeFilesAreValidYAML(t *testing.T) {
	for name, content := range ServiceMap {
		t.Run(name, func(t *testing.T) {
			var parsed map[string]any
			if err := yaml.Unmarshal([]byte(content), &parsed); err != nil {
				t.Errorf("compose file for %q is not valid YAML: %v", name, err)
			}
			// Every compose file should have a top-level "services" key
			if _, ok := parsed["services"]; !ok {
				t.Errorf("compose file for %q missing top-level 'services' key", name)
			}
		})
	}
}

// TestSGLangComposeRegistered ensures the sglang service is in the ServiceMap.
func TestSGLangComposeRegistered(t *testing.T) {
	if _, ok := ServiceMap["sglang"]; !ok {
		t.Fatal("sglang not found in ServiceMap")
	}
}

// TestGetAvailableServicesIncludesSGLang ensures sglang appears in the
// sorted available services list.
func TestGetAvailableServicesIncludesSGLang(t *testing.T) {
	available := GetAvailableServices()
	found := false
	for _, s := range available {
		if s == "sglang" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAvailableServices() = %v, want sglang to be included", available)
	}
}

// TestBonsaiComposeVRAMTuning guards the VRAM-bounding flags (citadel #567):
// without --ctx-size llama-server allocates Bonsai's full 262K context and pins
// ~21GB VRAM. The bounded context plus 4-bit KV cache (which requires flash
// attention) keep it near ~5-6GB. If these drift out of the command, VRAM usage
// silently regresses, so pin them here.
func TestBonsaiComposeVRAMTuning(t *testing.T) {
	content, ok := ServiceMap["bonsai"]
	if !ok {
		t.Fatal("bonsai not found in ServiceMap")
	}
	for _, flag := range []string{
		"--ctx-size",
		"--flash-attn on",
		"--cache-type-k q4_0",
		"--cache-type-v q4_0",
	} {
		if !strings.Contains(content, flag) {
			t.Errorf("bonsai compose command missing VRAM-tuning flag %q", flag)
		}
	}
}

// TestDiffusersComposeRegistered ensures the diffusers service is in the
// ServiceMap so `citadel init` writes services/diffusers.yml and a node can
// enable it (aceteam #4468).
func TestDiffusersComposeRegistered(t *testing.T) {
	if _, ok := ServiceMap["diffusers"]; !ok {
		t.Fatal("diffusers not found in ServiceMap")
	}
}

// TestGetAvailableServicesIncludesDiffusers ensures diffusers appears in the
// sorted available services list.
func TestGetAvailableServicesIncludesDiffusers(t *testing.T) {
	available := GetAvailableServices()
	found := false
	for _, s := range available {
		if s == "diffusers" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAvailableServices() = %v, want diffusers to be included", available)
	}
}

// TestDiffusersComposeContract verifies the embedded diffusers compose file
// satisfies the aceteam #4468 contract: the container listens on the contract
// port 7860 and the host mapping avoids the terminal server's default 7860.
func TestDiffusersComposeContract(t *testing.T) {
	content := ServiceMap["diffusers"]
	// Container/server port is the contract port.
	if !strings.Contains(content, ":7860") {
		t.Errorf("diffusers compose should map to container port 7860; got:\n%s", content)
	}
	// Host port must not be 7860 (that would collide with the terminal server).
	if strings.Contains(content, "\"7860:") {
		t.Errorf("diffusers compose host port must not be 7860 (collides with terminal server); got:\n%s", content)
	}
}

// TestBonsaiComposeRegistered ensures the bonsai service (PrismML Bonsai-27B via
// the llama.cpp fork) is in the ServiceMap so `citadel run --service bonsai`,
// the manifest, and SERVICE_START can find it.
func TestBonsaiComposeRegistered(t *testing.T) {
	if _, ok := ServiceMap["bonsai"]; !ok {
		t.Fatal("bonsai not found in ServiceMap")
	}
	found := false
	for _, s := range GetAvailableServices() {
		if s == "bonsai" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAvailableServices() does not include bonsai")
	}
}

// TestBonsaiComposeContract verifies bonsai is the one embedded service that
// BUILDS its image (not a prebuilt image:), so its build context Dockerfile must
// be materializable via WriteAuxFiles, and its host publish must defer to the
// citadel-owned env var.
func TestBonsaiComposeContract(t *testing.T) {
	content := ServiceMap["bonsai"]
	if !strings.Contains(content, "build:") {
		t.Errorf("bonsai compose should use a build: section (no prebuilt image is published)")
	}
	if !strings.Contains(content, "${CITADEL_BONSAI_HOST_PORT") {
		t.Errorf("bonsai compose must defer its host port to CITADEL_BONSAI_HOST_PORT")
	}
	// The build-context Dockerfile must be registered so it lands on the node.
	aux, ok := ServiceAuxFiles["bonsai"]
	if !ok {
		t.Fatal("ServiceAuxFiles missing bonsai; the build context Dockerfile would never materialize")
	}
	if _, ok := aux[filepath.Join("bonsai", "Dockerfile")]; !ok {
		t.Errorf("ServiceAuxFiles[bonsai] missing bonsai/Dockerfile")
	}
}

// TestWriteAuxFilesMaterializesBonsaiDockerfile proves WriteAuxFiles writes the
// build-context Dockerfile to disk (the fix for bonsai being the first
// build-based embedded service; a .yml-only materialization would fail
// `docker compose build`).
func TestWriteAuxFilesMaterializesBonsaiDockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAuxFiles(dir, "bonsai"); err != nil {
		t.Fatalf("WriteAuxFiles(bonsai): %v", err)
	}
	dockerfile := filepath.Join(dir, "bonsai", "Dockerfile")
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", dockerfile, err)
	}
	if !strings.Contains(string(data), "PrismML-Eng/llama.cpp") {
		t.Errorf("materialized Dockerfile does not reference the PrismML fork")
	}
	// No-op for an image-based service.
	if err := WriteAuxFiles(dir, "vllm"); err != nil {
		t.Fatalf("WriteAuxFiles(vllm) should be a no-op, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vllm")); !os.IsNotExist(err) {
		t.Errorf("WriteAuxFiles(vllm) should not create any files")
	}
}

// TestKokoroComposeRegistered ensures the kokoro TTS service is in the
// ServiceMap so `citadel run --service kokoro`, the manifest, and SERVICE_START
// can find it (aceteam#6104).
func TestKokoroComposeRegistered(t *testing.T) {
	if _, ok := ServiceMap["kokoro"]; !ok {
		t.Fatal("kokoro not found in ServiceMap")
	}
	found := false
	for _, s := range GetAvailableServices() {
		if s == "kokoro" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAvailableServices() does not include kokoro")
	}
}

// TestKokoroComposeContract verifies the embedded kokoro compose satisfies its
// contract: it uses the prebuilt image (no build context to materialize, unlike
// bonsai), defers its host publish to the citadel-owned CITADEL_TTS_HOST_PORT,
// and binds loopback only (the service has no auth of its own). The loopback
// prefix means the host token must NOT carry bonsai's `${VAR:?msg}` guard, since
// that smears across the host-port test parser's colon split.
func TestKokoroComposeContract(t *testing.T) {
	content, ok := ServiceMap["kokoro"]
	if !ok {
		t.Fatal("kokoro not found in ServiceMap")
	}
	if !strings.Contains(content, "ghcr.io/aceteam-ai/kokoro-service") {
		t.Errorf("kokoro compose should use the prebuilt kokoro-service image")
	}
	if strings.Contains(content, "build:") {
		t.Errorf("kokoro compose should NOT declare a build: section (it uses a prebuilt image)")
	}
	if !strings.Contains(content, "${CITADEL_TTS_HOST_PORT}") {
		t.Errorf("kokoro compose must defer its host port to a bare ${CITADEL_TTS_HOST_PORT} (no :?/:- guard, which breaks the loopback host-port parser)")
	}
	if !strings.Contains(content, "127.0.0.1:${CITADEL_TTS_HOST_PORT}:8080") {
		t.Errorf("kokoro compose must bind loopback only (127.0.0.1) -- the service has no auth of its own")
	}
	// kokoro must NOT need aux build-context files (it is image-based).
	if _, ok := ServiceAuxFiles["kokoro"]; ok {
		t.Errorf("ServiceAuxFiles should not contain kokoro; it uses a prebuilt image, not a build context")
	}
}

// NOTE: the embedded kokoro compose publishes exactly the citadel-assigned host
// port (services.TTSHostPort); that registry-vs-compose agreement is proven by
// internal/apps/hostport_collision_test.go, which resolves the loopback
// ${CITADEL_TTS_HOST_PORT} publish against the registry and cross-checks it.

// composeHostPorts returns the host-side ports declared in a compose file's
// `ports:` list entries ("HOST:CONTAINER"). Pure parse (no Docker) so the
// host-port collision assertions run in ordinary CI.
func composeHostPorts(t *testing.T, composeYAML string) []int {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeYAML), &doc); err != nil {
		t.Fatalf("compose is not valid YAML: %v", err)
	}
	// A citadel-managed host publish defers to ${CITADEL_*_HOST_PORT}; resolve
	// those tokens to the registry value so the collision assertions validate the
	// port citadel actually injects (services/ports.go).
	envVarHostPort := map[string]int{
		EnvLlamacppHostPort:   LlamacppHostPort,
		EnvVLLMHostPort:       VLLMHostPort,
		EnvExtractionHostPort: ExtractionHostPort,
		EnvDiffusersHostPort:  DiffusersHostPort,
		EnvBonsaiHostPort:     BonsaiHostPort,
		EnvTTSHostPort:        TTSHostPort,
	}
	var hosts []int
	for _, svc := range doc.Services {
		for _, mapping := range svc.Ports {
			// The host side is everything before the container colon, but a
			// ${CITADEL_*_HOST_PORT:?...} expansion carries its own colons, so
			// peel a leading ${...} group intact and resolve it via the registry.
			if strings.HasPrefix(mapping, "${") {
				if end := strings.IndexByte(mapping, '}'); end >= 0 {
					inner := mapping[2:end]
					varName := inner
					if c := strings.IndexByte(inner, ':'); c >= 0 {
						varName = inner[:c]
					}
					if port, ok := envVarHostPort[varName]; ok {
						hosts = append(hosts, port)
					}
				}
				continue
			}
			// "HOST:CONTAINER" (optionally "HOST:CONTAINER/proto"); the host side
			// is everything before the first colon.
			hostStr := mapping
			if i := strings.IndexByte(mapping, ':'); i >= 0 {
				hostStr = mapping[:i]
			}
			var p int
			if _, err := fmt.Sscanf(hostStr, "%d", &p); err == nil && p > 0 {
				hosts = append(hosts, p)
			}
		}
	}
	return hosts
}

// TestDiffusersHostPortNonColliding is the #415 regression guard: the diffusers
// host port must not collide with any other host port a citadel node binds.
// Concretely it must avoid:
//   - 7860 (terminal server / diffusers contract port),
//   - 8102 (TEI embeddings -- the exact collision reported in #415),
//   - the vllm/transcribe/tei 8100-8102 sequence, and
//   - the whole 8100-8199 range that internal/apps auto-allocates.
//
// It parses the actual `ports:` mapping rather than substring-matching so a
// future edit that reintroduces a bad host port fails here.
func TestDiffusersHostPortNonColliding(t *testing.T) {
	hosts := composeHostPorts(t, ServiceMap["diffusers"])
	if len(hosts) == 0 {
		t.Fatalf("diffusers compose declares no host port mapping; a provisioned endpoint would be unreachable (#415)")
	}
	for _, p := range hosts {
		switch {
		case p == 7860:
			t.Errorf("diffusers host port 7860 collides with the terminal server / contract port (#415)")
		case p == 8102:
			t.Errorf("diffusers host port 8102 collides with the TEI embedding service (#415)")
		case p >= 8100 && p <= 8199:
			t.Errorf("diffusers host port %d is inside the 8100-8199 range reserved by other services and internal/apps auto-allocation (#415)", p)
		}
	}
}

// TestKnownComposeHashesCoverCurrentTemplates verifies the generated
// KnownComposeHashes allowlist includes the sha256 of every CURRENT embedded
// template. This is the bootstrap safety net for pre-#426 nodes: a node freshly
// materialized by this binary but carrying no .citadel-managed.json stamp must
// be recognized as citadel-written (so the re-materialization sweep does not
// mis-flag it as operator-edited). If this fails, regenerate known_hashes.go.
func TestKnownComposeHashesCoverCurrentTemplates(t *testing.T) {
	for name, content := range ServiceMap {
		sum := sha256.Sum256([]byte(content))
		h := hex.EncodeToString(sum[:])
		if !KnownComposeHashes[name][h] {
			t.Errorf("KnownComposeHashes[%q] is missing the current template hash %s; regenerate services/known_hashes.go", name, h)
		}
	}
}
