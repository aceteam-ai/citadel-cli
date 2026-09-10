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

// TestTEIComposeContract guards the sovereign-embedding TEI module. The
// float32 + single-thread MKL/OMP/RAYON combination is load-bearing: float16 on
// CPU segfaults through Intel MKL (citadel-services#14), so if these drift the
// service crash-loops on start. The container serves :80 published to the fixed
// host port 8102 (the gateway's /v1/embeddings upstream); a drift there silently
// breaks discovery + the gateway proxy.
func TestTEIComposeContract(t *testing.T) {
	content, ok := ServiceMap["tei"]
	if !ok {
		t.Fatal("tei not found in ServiceMap")
	}
	for _, want := range []string{
		"Alibaba-NLP/gte-multilingual-base",
		"--dtype", "float32",
		"MKL_NUM_THREADS=1",
		"OMP_NUM_THREADS=1",
		"RAYON_NUM_THREADS=1",
		"127.0.0.1:8102:80",
		"cpu-1.6",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("tei compose missing required contract fragment %q", want)
		}
	}
}

// TestGetAvailableServicesIncludesTEI ensures tei is advertised as a deployable
// service so the fabric can schedule the sovereign-embedding module to a node.
func TestGetAvailableServicesIncludesTEI(t *testing.T) {
	for _, s := range GetAvailableServices() {
		if s == "tei" {
			return
		}
	}
	t.Errorf("GetAvailableServices() = %v, want tei to be included", GetAvailableServices())
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
		t.Errorf("kokoro compose must bind loopback only (127.0.0.1); the service has no auth of its own")
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

// TestOmniVoiceComposeRegistered ensures the omnivoice TTS service is in the
// ServiceMap so `citadel run --service omnivoice`, the manifest, and
// SERVICE_START can find it (citadel-cli#1007).
func TestOmniVoiceComposeRegistered(t *testing.T) {
	if _, ok := ServiceMap["omnivoice"]; !ok {
		t.Fatal("omnivoice not found in ServiceMap")
	}
	found := false
	for _, s := range GetAvailableServices() {
		if s == "omnivoice" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAvailableServices() does not include omnivoice")
	}
}

// TestOmniVoiceComposeContract verifies the embedded omnivoice compose
// satisfies its contract: it uses the citedel-inference-server `tts` image (no
// build context to materialize, mirroring kokoro/unlimited-ocr's "should NOT
// declare a build: section" assertion), defers its host publish to the
// citadel-owned CITADEL_OMNIVOICE_HOST_PORT with the bare (no :?/:- guard)
// token the loopback prefix requires, and binds loopback only (the service has
// no auth of its own).
func TestOmniVoiceComposeContract(t *testing.T) {
	content, ok := ServiceMap["omnivoice"]
	if !ok {
		t.Fatal("omnivoice not found in ServiceMap")
	}
	if !strings.Contains(content, "ghcr.io/aceteam-ai/citadel-inference-server:tts") {
		t.Errorf("omnivoice compose should use the citadel-inference-server tts image")
	}
	if strings.Contains(content, "build:") {
		t.Errorf("omnivoice compose should NOT declare a build: section (it uses a prebuilt image)")
	}
	if !strings.Contains(content, "${CITADEL_OMNIVOICE_HOST_PORT}") {
		t.Errorf("omnivoice compose must defer its host port to a bare ${CITADEL_OMNIVOICE_HOST_PORT} (no :?/:- guard, which breaks the loopback host-port parser)")
	}
	if !strings.Contains(content, "127.0.0.1:${CITADEL_OMNIVOICE_HOST_PORT}:8000") {
		t.Errorf("omnivoice compose must bind loopback only (127.0.0.1); the service has no auth of its own")
	}
	// omnivoice must NOT need aux build-context files (it is image-based).
	if _, ok := ServiceAuxFiles["omnivoice"]; ok {
		t.Errorf("ServiceAuxFiles should not contain omnivoice; it uses a prebuilt image, not a build context")
	}
	// Jason's decision (citadel-cli#1007 §6.2): OmniVoice is a GPU-only engine,
	// unlike kokoro -- fail loud on a CPU-only node rather than silently degrade
	// to an unusably slow CPU diffusion loop.
	if !strings.Contains(content, "driver: nvidia") {
		t.Errorf("omnivoice compose must declare the GPU reservation block (driver: nvidia); OmniVoice is not real-time on CPU")
	}
}

// loopbackBoundEngineHostPorts maps each engine ServiceMap entry that this
// test asserts is loopback-only to the bare (no `:?`/`:-` guard) host-port
// token its compose file must publish behind a literal "127.0.0.1:" prefix
// (aceteam-ai/aceteam#9523). sglang has no citadel-injected host-port var (its
// compose publishes the literal 30000), so its expected token is empty and the
// assertion below checks the literal "127.0.0.1:30000:" prefix directly instead.
var loopbackBoundEngineHostPorts = map[string]string{
	"vllm":          "${" + EnvVLLMHostPort + "}",
	"llamacpp":      "${" + EnvLlamacppHostPort + "}",
	"bonsai":        "${" + EnvBonsaiHostPort + "}",
	"unlimited-ocr": "${" + EnvUnlimitedOCRHostPort + "}",
	"kokoro":        "${" + EnvTTSHostPort + "}",
	"omnivoice":     "${" + EnvOmniVoiceHostPort + "}",
}

// TestEngineComposeFilesLoopbackBound is the aceteam-ai/aceteam#9523 contract
// test: every OpenAI-compatible inference engine compose file with no auth of
// its own must publish its host port on 127.0.0.1 only, using the bare-token
// idiom (no `:?`/`:-` guard, which would
// smear across this parser's colon handling; see the kokoro.yml/
// omnivoice.yml comments this pattern mirrors). Table-driven per Acceptance
// criterion 4 in the parent issue ("extend a TestEngineCacheDirsMatchComposeMounts
// -style test to assert the bind for every ServiceMap entry").
func TestEngineComposeFilesLoopbackBound(t *testing.T) {
	for name, token := range loopbackBoundEngineHostPorts {
		t.Run(name, func(t *testing.T) {
			content, ok := ServiceMap[name]
			if !ok {
				t.Fatalf("%q not found in ServiceMap", name)
			}
			want := "127.0.0.1:" + token + ":"
			if !strings.Contains(content, want) {
				t.Errorf("compose %q must publish its host port loopback-only via %q; got:\n%s", name, want, content)
			}
			// The guarded form must be absent. If present, either this
			// engine's compose was reverted to the old ${VAR:?msg} shape, or a
			// hand-edit reintroduced a guard that would break the loopback
			// host-port parsers (services/embed_test.go's composeHostPorts,
			// internal/apps/hostport_collision_test.go's hostPortField).
			if strings.Contains(content, token+":?") || strings.Contains(content, token+":-") {
				t.Errorf("compose %q must use the bare %q token (no :?/:- guard) behind the loopback prefix", name, token)
			}
		})
	}

	// sglang: no citadel-injected host-port var, so its loopback literal is
	// checked directly rather than via the token map above.
	sglang, ok := ServiceMap["sglang"]
	if !ok {
		t.Fatal("sglang not found in ServiceMap")
	}
	if !strings.Contains(sglang, "127.0.0.1:30000:30000") {
		t.Errorf("sglang compose must publish its host port loopback-only (127.0.0.1:30000:30000); got:\n%s", sglang)
	}
}

// nonLoopbackServiceMapAllowlist documents every services.ServiceMap entry
// that is NOT loopback-bound and why, so TestServiceMapBindSweep enforces
// "every current or future ServiceMap entry is loopback-bound, or has a
// recorded reason" rather than silently widening or narrowing over time. Do
// not add an entry here without a reason a reviewer can check against the
// actual compose file.
//
// services/compose/claudecode.yml and hermes.yml are NOT covered by this
// sweep (or listed here) even though they publish a 0.0.0.0 host port on
// disk: neither is `//go:embed`-ed into ServiceMap (verified: no
// ClaudecodeCompose/HermesCompose var in embed.go, and no other Go code
// references either path), so both are dead files in this repo. The live
// copies are the citadel-services catalog modules (services/claudecode,
// services/hermes), per this repo's CLAUDE.md.
var nonLoopbackServiceMapAllowlist = map[string]string{
	"ollama": "internal/apps/catalog.go sets OLLAMA_BASE_URL=http://host.docker.internal:11434 " +
		"for a catalog app; a container reaching the host via host.docker.internal lands on the " +
		"docker0 bridge gateway, not 127.0.0.1, so a loopback-only publish would break that consumer. " +
		"Tracked in aceteam-ai/citadel-cli#1023.",
	"extraction": "out of scope for aceteam-ai/aceteam#9523 (issue named vllm/sglang/llamacpp/bonsai/" +
		"unlimited-ocr/ollama only); same 0.0.0.0-with-no-auth shape, tracked as a broader-sweep " +
		"candidate in aceteam-ai/citadel-cli#1023.",
	"diffusers": "out of scope for aceteam-ai/aceteam#9523; same shape, tracked in aceteam-ai/citadel-cli#1023.",
	"transcribe": "out of scope for aceteam-ai/aceteam#9523; fixed native port (8101), not a citadel-" +
		"injected host-port var. Tracked in aceteam-ai/citadel-cli#1023.",
	"lmstudio": "out of scope for aceteam-ai/aceteam#9523; fixed native port (1234), not a citadel-" +
		"injected host-port var. Tracked in aceteam-ai/citadel-cli#1023.",
	"tei": "already loopback-bound (127.0.0.1:8102:80), not an oversight, just not matched by the " +
		"exact-token check below since it has no citadel-injected host-port var either.",
}

// TestServiceMapBindSweep sweeps EVERY current services.ServiceMap entry and
// asserts it is either loopback-bound (127.0.0.1: prefix on its ports: host
// side) or explicitly allowlisted with a reason
// (nonLoopbackServiceMapAllowlist). A new ServiceMap entry that publishes a
// host port on all interfaces with no allowlist reason fails here. This is
// the "assert the bind for every ServiceMap entry" acceptance criterion from
// aceteam-ai/aceteam#9523, scoped to today's actual fix (5 engines) plus a
// recorded, reviewable reason for every other entry rather than a silent
// pass.
func TestServiceMapBindSweep(t *testing.T) {
	for name, content := range ServiceMap {
		t.Run(name, func(t *testing.T) {
			var doc struct {
				Services map[string]struct {
					Ports []string `yaml:"ports"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
				t.Fatalf("compose %q is not valid YAML: %v", name, err)
			}
			hasHostPublish := false
			loopbackOnly := true
			for _, svc := range doc.Services {
				for _, spec := range svc.Ports {
					if !strings.Contains(spec, ":") {
						continue // container-port-only, no host publish
					}
					hasHostPublish = true
					if !strings.HasPrefix(spec, "127.0.0.1:") {
						loopbackOnly = false
					}
				}
			}
			if !hasHostPublish {
				return // nothing published on the host; nothing to check
			}
			if loopbackOnly {
				return
			}
			if reason, allowlisted := nonLoopbackServiceMapAllowlist[name]; allowlisted {
				if reason == "" {
					t.Errorf("nonLoopbackServiceMapAllowlist[%q] must carry a non-empty reason", name)
				}
				return
			}
			t.Errorf("compose %q publishes a host port on all interfaces with no auth-posture review; "+
				"either bind it loopback-only or add a reasoned entry to nonLoopbackServiceMapAllowlist", name)
		})
	}
}

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
		EnvOmniVoiceHostPort:  OmniVoiceHostPort,
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
