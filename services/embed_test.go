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
	available := availableServicesFor("linux")
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
	for _, s := range availableServicesFor("linux") {
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
	available := availableServicesFor("linux")
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
	for _, s := range availableServicesFor("linux") {
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
	for _, s := range availableServicesFor("linux") {
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
	for _, s := range availableServicesFor("linux") {
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

// bindHatchEngineHostPorts maps each engine ServiceMap entry that carries the
// aceteam-ai/citadel-cli#1023 bind escape hatch to (its bind env var, its bare
// host-port token). The compose publish is the two-substitution form
// ${CITADEL_<SVC>_BIND:-127.0.0.1}:<hostToken>:<cport>: the DEFAULT bind (env
// unset) is loopback (aceteam-ai/aceteam#9523's posture), and an operator can
// inject CITADEL_<SVC>_BIND=0.0.0.0 to open it to all interfaces. sglang is
// checked separately below (it has no ${...HOST_PORT} var; its host port is the
// literal 30000).
var bindHatchEngineHostPorts = map[string]struct {
	bindVar   string
	hostToken string
}{
	"vllm":          {EnvVLLMBind, "${" + EnvVLLMHostPort + "}"},
	"llamacpp":      {EnvLlamacppBind, "${" + EnvLlamacppHostPort + "}"},
	"bonsai":        {EnvBonsaiBind, "${" + EnvBonsaiHostPort + "}"},
	"unlimited-ocr": {EnvUnlimitedOCRBind, "${" + EnvUnlimitedOCRHostPort + "}"},
	// aceteam-ai/citadel-cli#1060 sweep: extraction/diffusers joined the hatch
	// with the same bare ${...HOST_PORT} token form (their :? guards were
	// dropped to mirror #1023's vllm/llamacpp exactly).
	"extraction": {EnvExtractionBind, "${" + EnvExtractionHostPort + "}"},
	"diffusers":  {EnvDiffusersBind, "${" + EnvDiffusersHostPort + "}"},
}

// literalLoopbackEngineHostPorts maps each engine ServiceMap entry hardcoded to
// the literal 127.0.0.1 prefix with NO bind hatch (kokoro/omnivoice are
// co-located-consumer-only; their compose comments forbid off-host reach) to its
// bare host-port token.
var literalLoopbackEngineHostPorts = map[string]string{
	"kokoro":    "${" + EnvTTSHostPort + "}",
	"omnivoice": "${" + EnvOmniVoiceHostPort + "}",
}

// TestEngineComposeFilesLoopbackBound is the aceteam-ai/aceteam#9523 +
// aceteam-ai/citadel-cli#1023 contract test: every OpenAI-compatible inference
// engine compose file with no auth of its own must, BY DEFAULT, publish its host
// port on 127.0.0.1 only. The 5 hatch engines do so via the two-substitution
// ${CITADEL_<SVC>_BIND:-127.0.0.1} form (default loopback, opt-in all-interfaces
// via CITADEL_<SVC>_BIND=0.0.0.0); kokoro/omnivoice via the literal prefix.
func TestEngineComposeFilesLoopbackBound(t *testing.T) {
	// Hatch engines: assert the two-substitution form is present, its DEFAULT
	// resolves loopback-only, and a bind:all env flips it to all-interfaces.
	for name, spec := range bindHatchEngineHostPorts {
		t.Run(name, func(t *testing.T) {
			content, ok := ServiceMap[name]
			if !ok {
				t.Fatalf("%q not found in ServiceMap", name)
			}
			want := "${" + spec.bindVar + ":-127.0.0.1}:" + spec.hostToken + ":"
			if !strings.Contains(content, want) {
				t.Errorf("compose %q must publish via the #1023 bind hatch %q; got:\n%s", name, want, content)
			}
			// Registry agreement: the bind var must be the one services.BindEnv
			// resolves for this service, so the compose token and the injector
			// can never drift.
			if got, ok := BindEnvVarName(name); !ok || got != spec.bindVar {
				t.Errorf("BindEnvVarName(%q) = (%q,%v), want (%q,true)", name, got, ok, spec.bindVar)
			}
			// Default (env unset) is loopback-only.
			if lb, has := ComposePublishesLoopbackOnly(content, nil); !lb || !has {
				t.Errorf("compose %q default bind must be loopback-only; got (loopbackOnly=%v,hasPublish=%v)", name, lb, has)
			}
			// bind:all env opens it to all interfaces.
			if lb, _ := ComposePublishesLoopbackOnly(content, map[string]string{spec.bindVar: AllInterfacesBindAddr}); lb {
				t.Errorf("compose %q with %s=0.0.0.0 must NOT be loopback-only", name, spec.bindVar)
			}
		})
	}

	// Literal-loopback engines (no hatch): assert the 127.0.0.1 literal prefix
	// and that no bind var is registered for them.
	for name, token := range literalLoopbackEngineHostPorts {
		t.Run(name, func(t *testing.T) {
			content, ok := ServiceMap[name]
			if !ok {
				t.Fatalf("%q not found in ServiceMap", name)
			}
			want := "127.0.0.1:" + token + ":"
			if !strings.Contains(content, want) {
				t.Errorf("compose %q must publish its host port loopback-only via %q; got:\n%s", name, want, content)
			}
			if _, ok := BindEnvVarName(name); ok {
				t.Errorf("%q must NOT carry a bind hatch (co-located-consumer-only)", name)
			}
		})
	}

	// Literal-host-port hatch engines: bind hatch behind a LITERAL host port (no
	// ${...HOST_PORT} var). sglang (30000) plus the aceteam-ai/citadel-cli#1060
	// sweep of transcribe (8101) and lmstudio (1234).
	for name, spec := range bindHatchLiteralHostPortEngines {
		t.Run(name, func(t *testing.T) {
			content, ok := ServiceMap[name]
			if !ok {
				t.Fatalf("%q not found in ServiceMap", name)
			}
			want := "${" + spec.bindVar + ":-127.0.0.1}:" + spec.hostPort + ":" + spec.hostPort
			if !strings.Contains(content, want) {
				t.Errorf("%q compose must publish via the #1023 bind hatch (%q); got:\n%s", name, want, content)
			}
			if got, ok := BindEnvVarName(name); !ok || got != spec.bindVar {
				t.Errorf("BindEnvVarName(%q) = (%q,%v), want (%q,true)", name, got, ok, spec.bindVar)
			}
			if lb, has := ComposePublishesLoopbackOnly(content, nil); !lb || !has {
				t.Errorf("%q default bind must be loopback-only; got (loopbackOnly=%v,hasPublish=%v)", name, lb, has)
			}
			if lb, _ := ComposePublishesLoopbackOnly(content, map[string]string{spec.bindVar: AllInterfacesBindAddr}); lb {
				t.Errorf("%q with %s=0.0.0.0 must NOT be loopback-only", name, spec.bindVar)
			}
		})
	}
}

// bindHatchLiteralHostPortEngines maps each engine ServiceMap entry that carries
// the aceteam-ai/citadel-cli#1023 bind hatch behind a LITERAL host port (no
// ${...HOST_PORT} var) to (its bind env var, its literal host port). The compose
// publish is ${CITADEL_<SVC>_BIND:-127.0.0.1}:<port>:<port>.
var bindHatchLiteralHostPortEngines = map[string]struct {
	bindVar  string
	hostPort string
}{
	"sglang":     {EnvSGLangBind, "30000"},
	"transcribe": {EnvTranscribeBind, "8101"},
	"lmstudio":   {EnvLMStudioBind, "1234"},
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
	"ollama": "aceteam-ai/citadel-cli#1023: bind hatch DEFAULTS to all-interfaces " +
		"(${CITADEL_OLLAMA_BIND:-0.0.0.0}) because internal/apps/catalog.go sets " +
		"OLLAMA_BASE_URL=http://host.docker.internal:11434 for a catalog app; a container reaching " +
		"the host via host.docker.internal lands on the docker0 bridge gateway, not 127.0.0.1, so a " +
		"loopback-only default would break that consumer. An operator can tighten it with " +
		"`bind: loopback` (CITADEL_OLLAMA_BIND=127.0.0.1); doctor/status flag the default as LAN-exposed.",
	// extraction/diffusers/transcribe/lmstudio moved to a loopback default in the
	// aceteam-ai/citadel-cli#1060 sweep (bind hatch), so they are no longer
	// non-loopback and hit the loopbackOnly early-return in TestServiceMapBindSweep.
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
			// Resolve the DEFAULT bind (empty env) through the single authority
			// ComposePublishesLoopbackOnly so the #1023 two-substitution form
			// ${CITADEL_<SVC>_BIND:-127.0.0.1} reads as loopback-by-default and
			// ollama's ${...:-0.0.0.0} reads as all-interfaces-by-default.
			loopbackOnly, hasHostPublish := ComposePublishesLoopbackOnly(content, nil)
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
			// ComposePortHostToken (services/bind.go) is the single authority for
			// pulling the host-port field out of any port-spec form, including the
			// #1023 two-substitution ${BIND:-127.0.0.1}:${HOST}:<cport> shape.
			tok := ComposePortHostToken(mapping)
			if tok == "" {
				continue // container-port-only, no host publish
			}
			// A ${CITADEL_*_HOST_PORT} host token resolves via the registry so the
			// collision assertions validate the port citadel actually injects.
			if strings.HasPrefix(tok, "${") {
				inner := strings.TrimSuffix(strings.TrimPrefix(tok, "${"), "}")
				varName := inner
				if c := strings.IndexByte(inner, ':'); c >= 0 {
					varName = inner[:c]
				}
				if port, ok := envVarHostPort[varName]; ok {
					hosts = append(hosts, port)
				}
				continue
			}
			var p int
			if _, err := fmt.Sscanf(tok, "%d", &p); err == nil && p > 0 {
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
