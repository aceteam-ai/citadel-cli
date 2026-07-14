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
)

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
	GatewayPort:       "gateway/status-server",
	GatewayHTTPSPort:  "gateway-https",
	ControlMTLSPort:   "control-mtls",
	TEIEmbeddingPort:  "tei-embeddings",
	VNCWebsockifyPort: "vnc-websockify",
	VNCPort:           "vnc-rfb",
	DeskstreamPort:    "deskstream-h264",
	TerminalPort:      "terminal-server",
	LiveKitWSPort:     "livekit-signaling",
	LiveKitICETCPPort: "livekit-ice-tcp",
	LiveKitUDPMuxPort: "livekit-udp-mux",
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
