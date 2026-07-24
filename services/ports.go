// services/ports.go
//
// Citadel owns host-port allocation for the pre-packaged service/module compose
// files under services/compose/*.yml. Before this registry existed, each
// compose template hardcoded the HOST side of its `ports:` publish, so
// containers dictated host ports and collided with one another and with
// citadel's own listeners (gateway 8080, gateway-HTTPS 8443, status server 8080,
// the apps catalog's dynamic 8100-8199 range, and the TEI embedding upstream
// 8102).
//
// The fix: templates defer the host port to an env var that citadel supplies
// (e.g. `127.0.0.1:${CITADEL_LLAMACPP_HOST_PORT}:8080`). This file is the single
// authoritative source of those host ports. Every `docker compose up` site that
// can bring up one of these services must inject these variables into the
// process environment (see EnvVars) so `${CITADEL_*_HOST_PORT}` substitution
// resolves. Container ports are unchanged — only the host publish moves.
//
// Host ports for these services live in a dedicated 8200+ block, deliberately
// clear of BOTH citadel's own reserved ports AND the apps catalog's dynamic
// 8100-8199 allocation range (internal/apps/state.go), so a dynamically
// allocated app can never collide with a module's fixed port.
package services

import "fmt"

// Host-port env-var names referenced by services/compose/*.yml. Kept as
// exported constants so consumers (Go code that reaches these services) and the
// compose templates share one spelling.
const (
	EnvLlamacppHostPort   = "CITADEL_LLAMACPP_HOST_PORT"
	EnvVLLMHostPort       = "CITADEL_VLLM_HOST_PORT"
	EnvExtractionHostPort = "CITADEL_EXTRACTION_HOST_PORT"
	EnvDiffusersHostPort  = "CITADEL_DIFFUSERS_HOST_PORT"
	// EnvClaudecodeHostPort carries the host port for the claudecode
	// agent-runtime module (aceteam-ai/citadel-cli#458). Unlike the four inference
	// services above, claudecode ships as an installable catalog MODULE (its
	// compose lives in aceteam-ai/citadel-services, not the embedded ServiceMap),
	// but it is registered here so its host port is injected by the same
	// HostPortEnv() mechanism. That is what lets a second agent-runtime module
	// (Hermes/OpenClaw, aceteam#4591) claim the next slot in the 8200 block instead
	// of every module hardcoding a literal 8204 and colliding.
	EnvClaudecodeHostPort = "CITADEL_CLAUDECODE_HOST_PORT"
	// EnvMeetingdHostPort / EnvMeetingCDPHostPort carry the two host ports for the
	// meeting media-stack MODULE (aceteam-ai/citadel-cli#514). Like claudecode, the
	// meeting module's compose lives in aceteam-ai/citadel-services (not the
	// embedded ServiceMap), but both ports are registered here so the module's two
	// loopback listeners -- the meetingd control API and the Chromium CDP endpoint
	// -- claim fixed slots in the 8200 block and no future module hardcodes over
	// them. Fable's design named 8102/9223, but 8102 is already TEIEmbeddingPort;
	// the container-internal ports stay 8102/9223 while the HOST publish moves into
	// the 8200 registry block below.
	EnvMeetingdHostPort   = "CITADEL_MEETINGD_HOST_PORT"
	EnvMeetingCDPHostPort = "CITADEL_MEETING_CDP_HOST_PORT"
	// EnvGotenbergHostPort carries the host port for the gotenberg
	// document-conversion module (aceteam-ai/citadel-services#10). Like
	// claudecode and meeting, gotenberg's compose lives in
	// aceteam-ai/citadel-services (not the embedded ServiceMap), but it is
	// registered here so `citadel module install gotenberg` injects the port via
	// the same HostPortEnv() mechanism.
	EnvGotenbergHostPort = "CITADEL_GOTENBERG_HOST_PORT"
	// EnvBonsaiHostPort carries the host port for the bonsai inference service
	// (PrismML Bonsai-27B via the llama.cpp fork). Unlike claudecode/meeting/
	// gotenberg, bonsai is an EMBEDDED ServiceMap compose (services/compose/
	// bonsai.yml), so its compose defers the host publish to this var exactly
	// like llamacpp/vllm.
	EnvBonsaiHostPort = "CITADEL_BONSAI_HOST_PORT"
	// EnvTTSHostPort carries the host port for the kokoro text-to-speech service
	// (Kokoro-82M via an OpenAI-compatible HTTP API). Like bonsai it is an
	// EMBEDDED ServiceMap compose (services/compose/kokoro.yml), so its compose
	// defers the host publish to this var. The var is spelled CITADEL_TTS_HOST_PORT
	// (the generic engine name, `tts`) rather than CITADEL_KOKORO_HOST_PORT because
	// the citadel-services catalog module's compose already reads it under that
	// name; the registry KEY stays "kokoro" (the implementation name), mirroring
	// meeting's key/env-var split (key "meeting" / EnvMeetingdHostPort).
	EnvTTSHostPort = "CITADEL_TTS_HOST_PORT"
	// EnvFrigateHostPort carries the host port for Frigate's web UI/API in the nvr
	// camera-NVR module (aceteam-ai/citadel-cli#597). Like claudecode/meeting/
	// gotenberg, the nvr module's compose lives in the catalog repo
	// (aceteam-ai/citadel-services), NOT the embedded ServiceMap, but the port is
	// registered here so no other module can hardcode over it and so
	// `citadel module install nvr` injects it via the same HostPortEnv() mechanism.
	// The registry KEY is the module name "nvr" while the env var is spelled for the
	// container it maps to (frigate) — the same key/env split as meeting.
	EnvFrigateHostPort = "CITADEL_FRIGATE_HOST_PORT"
)

// Citadel-assigned host ports for the pre-packaged compose services. These are
// the ports the containers publish ON THE HOST; the in-container ports are
// unchanged and defined by each compose file's command/args.
//
// Only services whose old hardcoded host port collided are managed here.
// Services that already sat on a unique, well-known host port (ollama 11434,
// sglang 30000, lmstudio 1234, transcribe 8101) keep their native port and are
// still covered by the collision guard test so future edits can't reintroduce a
// clash.
const (
	// llamacpp: was host 8080 (collided with gateway + status server).
	LlamacppHostPort = 8200
	// vllm: was host 8100 (collided with extraction and the apps range).
	VLLMHostPort = 8201
	// extraction: was host 8100 (collided with vllm and the apps range).
	ExtractionHostPort = 8202
	// diffusers: was host 8102 (collided with the TEI embedding upstream).
	DiffusersHostPort = 8203
	// claudecode: the first agent-runtime MODULE (#458). Next free slot in the
	// 8200 block, above the apps 8100-8199 range. The wrapper's internal contract
	// port is 8787; this is the host publish. Kept in this registry so the next
	// agent-runtime module (Hermes/OpenClaw) is allocated 8205 rather than
	// re-hardcoding 8204.
	ClaudecodeHostPort = 8204
	// storage: the on-node S3-compatible object store (VersityGW, #469). It is
	// NOT a compose service and has no ${CITADEL_*_HOST_PORT} env var -- the
	// `citadel storage` command constructs its `docker run` publish directly from
	// this constant (internal/storage). It sits above the earmarked 8205
	// Hermes/OpenClaw slot so a fixed, reboot-stable port is signed into S3
	// presigned URLs without a persisted-port crash-loop risk. Kept in the
	// registry so future allocations skip it and the collision guard covers it.
	StorageHostPort = 8206
	// meeting: the meeting media-stack module (#514) publishes TWO loopback
	// listeners, so it takes the next two free slots after the earmarked 8205
	// Hermes/OpenClaw slot and storage's 8206. The container-internal ports are
	// 8102 (meetingd) and 9223 (CDP); these are the HOST publish. Both are bound to
	// 127.0.0.1 by the compose -- the only consumer is the co-located citadel
	// process.
	MeetingdHostPort   = 8207
	MeetingCDPHostPort = 8208
	// gotenberg: the document-conversion module (LibreOffice + Chromium -> PDF,
	// citadel-services#10) that unblocks Sovereign Sign P2 (aceteam#5793) --
	// sovereign DOCX->PDF conversion on the customer's own node. Next free slot
	// in the 8200 block after meeting's 8207/8208. The container-internal port
	// is 3000; this is the HOST publish, bound to 127.0.0.1 only (Gotenberg has
	// no auth of its own).
	GotenbergHostPort = 8209
	// bonsai: PrismML Bonsai-27B (1-bit Qwen3.6-27B) served by an
	// OpenAI-compatible llama-server built from the PrismML llama.cpp fork
	// (services/compose/bonsai.yml). Next free slot in the 8200 block after
	// gotenberg's 8209 (8205 stays earmarked for Hermes/OpenClaw). It is an
	// embedded ServiceMap compose, so its container serves on :8080 and this is
	// the HOST publish.
	BonsaiHostPort = 8210
	// kokoro: Kokoro-82M text-to-speech served over an OpenAI-compatible HTTP API
	// (services/compose/kokoro.yml), the synthesis counterpart to the whisper
	// transcribe sidecar. Next free slot in the 8200 block after bonsai's 8210
	// (8205 stays earmarked for Hermes/OpenClaw). It is an embedded ServiceMap
	// compose, so its container serves on :8080 and this is the HOST publish,
	// bound to 127.0.0.1 only (the service has no auth of its own and its sole
	// consumer is the co-located citadel worker).
	//
	// NOTE: the citadel-services kokoro module (README, service.yaml ports.host
	// and health_check.port) still names 8210 for this service, written before
	// bonsai claimed 8210 here. citadel-cli's registry is authoritative for the
	// injected host port; aligning citadel-services to 8211 is a follow-up in
	// that repo.
	TTSHostPort = 8211
	// frigate: the Frigate web UI/API for the nvr camera-NVR module (#597). Next
	// free slot in the 8200 block after kokoro's 8211. host 8212 -> container 5000
	// (Frigate's native web port); this is the HOST publish. The nvr module's
	// compose lives in citadel-services (like meeting/gotenberg), so this registry
	// is the only thing stopping a future module from hardcoding over 8212.
	FrigateHostPort = 8212
)

// WyzeBridgeRTSPPort is docker-wyze-bridge's RTSP server port in the nvr module
// (#597). wyze-bridge MUST run with host networking (TUTK P2P needs LAN broadcast
// + UDP hole-punching; Docker bridge NAT breaks camera discovery), so it binds
// this port directly ON THE HOST — outside Docker's publish bookkeeping and
// outside the env-var-substitution registry above, exactly like the LiveKit SFU.
// It is reserved below (ReservedCitadelPorts) so no app allocation or module
// publish can claim the port wyze-bridge will bind and that Frigate pulls RTSP
// from (via host.docker.internal:8554).
const WyzeBridgeRTSPPort = 8554

// ServiceHostPorts maps service name -> citadel-assigned host port. Most entries
// are compose services whose host publish citadel owns via env-var substitution;
// "storage" is the exception -- it has no compose file and is consumed directly
// by the storage command's `docker run` construction. The collision guard test
// unions this with the apps catalog and the parsed compose files to prove no two
// host ports clash.
var ServiceHostPorts = map[string]int{
	"llamacpp":    LlamacppHostPort,
	"vllm":        VLLMHostPort,
	"extraction":  ExtractionHostPort,
	"diffusers":   DiffusersHostPort,
	"claudecode":  ClaudecodeHostPort,
	"storage":     StorageHostPort,
	"meeting":     MeetingdHostPort,
	"meeting-cdp": MeetingCDPHostPort,
	"gotenberg":   GotenbergHostPort,
	"bonsai":      BonsaiHostPort,
	"kokoro":      TTSHostPort,
	"nvr":         FrigateHostPort,
}

// serviceHostPortEnv maps each managed service to the compose env-var that
// carries its host port.
var serviceHostPortEnv = map[string]string{
	"llamacpp":    EnvLlamacppHostPort,
	"vllm":        EnvVLLMHostPort,
	"extraction":  EnvExtractionHostPort,
	"diffusers":   EnvDiffusersHostPort,
	"claudecode":  EnvClaudecodeHostPort,
	"meeting":     EnvMeetingdHostPort,
	"meeting-cdp": EnvMeetingCDPHostPort,
	"gotenberg":   EnvGotenbergHostPort,
	"bonsai":      EnvBonsaiHostPort,
	"kokoro":      EnvTTSHostPort,
	"nvr":         EnvFrigateHostPort,
}

// HostPortEnv returns "KEY=value" entries for every citadel-managed host port,
// suitable for appending to a docker compose invocation's environment so
// `${CITADEL_*_HOST_PORT}` substitutions in the compose templates resolve.
//
// It returns ALL managed vars unconditionally (not just the one for a given
// service) so any `docker compose up` site can call it once and every managed
// compose file it might bring up will substitute correctly.
func HostPortEnv() []string {
	env := make([]string, 0, len(serviceHostPortEnv))
	for svc, key := range serviceHostPortEnv {
		env = append(env, fmt.Sprintf("%s=%d", key, ServiceHostPorts[svc]))
	}
	return env
}

// ManagedServiceHostPort returns the citadel-assigned host port for a managed
// service (llamacpp/vllm/extraction) and whether it is managed. Consumers that
// reach these services over localhost should route through this so they hit the
// current host port instead of a hardcoded literal.
func ManagedServiceHostPort(name string) (int, bool) {
	port, ok := ServiceHostPorts[name]
	return port, ok
}

// SGLangHostPort is sglang's native host port (services/compose/sglang.yml).
// It never collided with anything, so it is not env-var-managed like the 8200
// block; the constant exists so consumers (the heartbeat stats scraper,
// internal/jobs/llm_inference.go) share one spelling instead of hardcoding
// 30000.
const SGLangHostPort = 30000

// InferenceMetricsPorts maps the inference engines that expose a Prometheus
// /metrics endpoint on their serving port to the host port citadel publishes
// them on. This is the discovery source for the heartbeat stats scraper
// (internal/pulse, citadel-cli#587): scrape targets come from this registry,
// never from hardcoded literals. Engines without a Prometheus endpoint
// (ollama, llamacpp, lmstudio) are deliberately absent.
func InferenceMetricsPorts() map[string]int {
	return map[string]int{
		"vllm":   VLLMHostPort,
		"sglang": SGLangHostPort,
	}
}

// Citadel's own listeners. These are NOT module ports; they belong to
// citadel-internal processes and must never be handed to a module compose file
// or dynamically allocated to an app.
const (
	// GatewayPort is the default plain gateway port (cmd/gateway.go) and the
	// default status-server port (citadel work --status-port). Shared across
	// those contexts by design.
	GatewayPort = 8080
	// GatewayHTTPSPort is the default HTTPS gateway port (cmd/serve.go).
	GatewayHTTPSPort = 8443
	// ControlMTLSPort is the coordinator mTLS control listener
	// (internal/status.DefaultControlPort, overridable via CITADEL_CONTROL_PORT).
	// It originally defaulted to 8443 and collided with GatewayHTTPSPort on the
	// mesh IP (#504): the control listener bound first, the gateway's tsnet bind
	// failed ("listener already open"), and every mesh gateway route (/vnc,
	// /terminal, /modules/*) silently went dark fleet-wide. The coordinator
	// (aceteam python-backend routes/fabric_relay.py) dials this port first and
	// falls back to legacy 8443 for nodes that predate the move.
	ControlMTLSPort = 8444
	// TEIEmbeddingPort is the local TEI embedding service, wired as the
	// gateway's /v1/embeddings upstream (cmd/serve.go --embedding-port,
	// internal/jobs/embedding_handler.go). It sits INSIDE the apps
	// auto-allocation range (8100-8199), so app port allocation must skip it.
	TEIEmbeddingPort = 8102
	// TranscribePort is the whisper sidecar's host port (services/compose/
	// transcribe.yml). It also sits inside the apps range, so allocation must
	// skip it. It is left at its native 8101 because it never collided with
	// another compose service.
	TranscribePort = 8101
	// VNCWebsockifyPort is the noVNC websockify bridge (cmd/work.go
	// --gateway-vnc-port).
	VNCWebsockifyPort = 6080
	// VNCPort is the raw RFB VNC port (internal/desktop, internal/platform/vnc).
	VNCPort = 5900
	// DeskstreamPort is the H.264 desktop stream port (internal/deskstream).
	DeskstreamPort = 5910
	// TerminalPort is the local terminal server port (cmd/work.go
	// --terminal-port, internal/terminal/config.go). The platform relay dials
	// ws://<vpn_ip>:7860, so this is a live mesh port a module must not take.
	TerminalPort = 7860
	// LiveKit SFU ports (voice huddles). The livekit catalog module
	// (aceteam-ai/citadel-services services/livekit) runs with
	// `network_mode: host`, so it binds these on the host directly — outside
	// Docker's publish bookkeeping and outside this registry's env-var
	// substitution. They are reserved here so no app allocation or module/
	// payload publish can claim a port the SFU's own config will bind. The
	// platform relay dials ws://<vpn_ip>:7880 for signaling, so 7880 is a live
	// mesh port just like TerminalPort.
	LiveKitWSPort     = 7880
	LiveKitICETCPPort = 7881
	LiveKitUDPMuxPort = 7882
)

// ReservedCitadelPorts is the set of host ports owned by citadel's own
// processes. No module compose file and no dynamically allocated app may use
// any of these. The collision guard test asserts the module registry, the apps
// catalog, and the parsed compose files all avoid this set.
var ReservedCitadelPorts = map[int]string{
	GatewayPort:        "gateway/status-server",
	GatewayHTTPSPort:   "gateway-https",
	ControlMTLSPort:    "control-mtls",
	TEIEmbeddingPort:   "tei-embeddings",
	VNCWebsockifyPort:  "vnc-websockify",
	VNCPort:            "vnc-rfb",
	DeskstreamPort:     "deskstream-h264",
	TerminalPort:       "terminal-server",
	LiveKitWSPort:      "livekit-signaling",
	LiveKitICETCPPort:  "livekit-ice-tcp",
	LiveKitUDPMuxPort:  "livekit-udp-mux",
	WyzeBridgeRTSPPort: "wyze-bridge-rtsp",
}

// AppsPortRange is the inclusive range apps auto-allocate host ports from
// (internal/apps.AllocatePort). Module ports live above this range.
const (
	AppsPortRangeStart = 8100
	AppsPortRangeEnd   = 8199
)

// InRangeReservedHostPorts returns the citadel-reserved host ports that fall
// inside the apps auto-allocation range, so app allocation can skip them.
func InRangeReservedHostPorts() map[int]bool {
	reserved := make(map[int]bool)
	for port := range ReservedCitadelPorts {
		if port >= AppsPortRangeStart && port <= AppsPortRangeEnd {
			reserved[port] = true
		}
	}
	// TranscribePort is a compose service (not in ReservedCitadelPorts) that
	// nonetheless occupies a port inside the apps range.
	if TranscribePort >= AppsPortRangeStart && TranscribePort <= AppsPortRangeEnd {
		reserved[TranscribePort] = true
	}
	return reserved
}
