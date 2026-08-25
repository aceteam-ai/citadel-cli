package status

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	nativesvc "github.com/aceteam-ai/citadel-cli/internal/services"
	"github.com/aceteam-ai/citadel-cli/services"
	"gopkg.in/yaml.v3"
)

// idleCapableEngines lists the serving engines for which idle detection has a
// reliable SCRAPED request signal. Currently only vLLM exposes the Prometheus
// request counters + running/waiting gauges the IdleTracker scrapes. Extend
// this as other engines (sglang, llama.cpp) grow comparable metrics.
//
// This is NOT the only idle/last-request signal in the heartbeat: every
// running service/app also gets a last_request_at fallback from locally-
// recorded node-routed requests (request_recorder.go, citadel #691), plus the
// #433 network-activity/footprint heuristics in footprint.go. See
// Collector.applyNodeRoutedRequestSignal for the merge order (it runs last, on
// the fully-assembled status, so it applies uniformly regardless of which of
// these engine lists produced a given entry).
var idleCapableEngines = []string{"vllm"}

// managedProbeEngines lists the managed serving engines the heartbeat path
// probes for a live signal: an idle signal (idleCapableEngines) and/or the
// loaded model(s) over the engine's local HTTP API (#529). It must remain a
// superset of idleCapableEngines (guarded by a test) so extending the idle
// list never silently drops an engine from the heartbeat.
//
// bonsai (PrismML Bonsai-27B on the llama.cpp fork, host port 8210) exposes the
// same OpenAI-compatible /v1/models API as llama.cpp, so probing it surfaces the
// served GGUF in the heartbeat's services[].models — which is how the inference
// gateway learns a node is serving Bonsai and can route to it (backend=bonsai).
//
// unlimited-ocr (Baidu Unlimited-OCR served by vLLM, host port 8213) is likewise
// an OpenAI-compatible /v1 engine — it just takes image_url input on
// /v1/chat/completions — so probing it surfaces baidu/Unlimited-OCR in the
// heartbeat and lets the gateway/mesh route document-OCR requests to it by model.
var managedProbeEngines = []string{"vllm", "ollama", "llamacpp", "bonsai", "unlimited-ocr"}

// collectManagedEngineStatus reports running managed serving engines (from the
// embedded services.ServiceMap) so their telemetry reaches the heartbeat even
// when no manifest-driven service config was passed to the collector (the
// common heartbeat case, where c.services is nil). Each entry carries the
// per-service idle signal when the engine's metrics are scrapeable (citadel
// #416) and the model(s) the engine can serve, discovered from the engine's
// local API (citadel #529): e.g. vLLM/llama.cpp `GET /v1/models` (the loaded
// model), ollama `GET /api/tags` (every pulled model, all auto-loadable on
// request; citadel-cli#606).
//
// It emits an entry for every engine that is actually running. An engine that
// answered a probe carries Health=ok plus its models/idle signal; one whose
// container is up but not answering yet (still loading weights, HTTP not up)
// carries Health=starting with no models and no idle signal (aceteam#7148).
// Dropping the unresponsive case is what made a just-deployed engine invisible
// in the fabric UI while `service_status` reported it running; "running but not
// ready yet" is real, reportable state, and callers that need readiness read
// Health. Probe failures never fail the collection: each probe is bounded by
// ModelDiscoveryTimeout and a failure simply leaves the corresponding field
// empty.
//
// running is the set of embedded-service names whose "citadel-<name>" container
// is up, collected once per heartbeat by runningEmbeddedServices.
func (c *Collector) collectManagedEngineStatus(running map[string]bool) []ServiceInfo {
	ctx := context.Background()
	var out []ServiceInfo

	for _, name := range managedProbeEngines {
		port, isRunning := enginePortIfRunning(running, name)
		if !isRunning || port <= 0 {
			continue
		}

		info := ServiceInfo{
			Name:   name,
			Type:   ServiceTypeLLM,
			Status: ServiceStatusRunning,
			Port:   port,
			Health: HealthStatusOK,
		}
		responded := false

		if c.idleTracker != nil && engineInList(idleCapableEngines, name) {
			if state, ok := c.idleTracker.Observe(ctx, name, name, port); ok {
				idle := state
				info.IdleState = &idle
				responded = true
			}
		}

		if c.modelDiscovery != nil {
			mctx, cancel := context.WithTimeout(ctx, ModelDiscoveryTimeout)
			models, err := c.modelDiscovery.DiscoverModels(mctx, name, port)
			cancel()
			if err == nil {
				info.Models = models
				responded = true
			}
		}

		// The node-routed request fallback (citadel #691) is applied centrally in
		// applyNodeRoutedRequestSignal (collector.go), after every producer
		// (including this one) has run and Health has settled -- an entry that
		// stays "starting" below is skipped there too.
		if !responded {
			info.Health = HealthStatusStarting
			info.Models = nil
			info.IdleState = nil
		}
		out = append(out, info)
	}
	return out
}

// embeddingProbeServices lists the OpenAI-compatible embedding services the
// heartbeat probes on this node. Unlike LLM engines (managedProbeEngines), they
// are advertised with Type=embedding and are NEVER offered to the gateway chat
// router: the sovereign RAG path (aceteam) discovers them via the "tei"/
// "embedding" service marker and reaches them through the gateway's
// /v1/embeddings upstream, not /v1/chat/completions.
var embeddingProbeServices = []string{"tei"}

// collectEmbeddingServiceStatus reports running embedding services as
// ServiceInfo with Type=embedding. This is the discovery signal the
// sovereign-embeddings backend (_find_tei_node) matches on: a node advertising a
// READY "tei" service becomes eligible to embed on its own model. Kept separate
// from collectManagedEngineStatus so an embedding server is never mistaken for a
// chat LLM (whose idle probe and chat-router listing do not apply).
//
// Each ready entry carries the model the server is ACTUALLY serving, read from
// the engine's own /info (citadel-cli#690). Reporting no models let a stopped
// vllm's <name>.env default claim the embedding model instead, so the platform
// credited a dead engine and reasoned about the wrong VRAM cost and lifecycle
// for both.
//
// running is the set of embedded-service names whose "citadel-<name>" container
// is up, enumerated once per heartbeat by runningEmbeddedServices. It is only
// half the running test: enginePortIfRunning falls back to the native probe, so
// an embedding server installed as a systemd unit or a bare `serve` process
// counts too (citadel-cli#690) instead of reporting a false negative on exactly
// the consumer-grade box this product targets.
func (c *Collector) collectEmbeddingServiceStatus(running map[string]bool) []ServiceInfo {
	var lister embeddingModelLister
	if c.modelDiscovery != nil {
		lister = c.modelDiscovery
	}
	portIfRunning := func(name string) (int, bool) { return enginePortIfRunning(running, name) }
	return collectEmbeddingServices(context.Background(), portIfRunning, embeddingServiceHealthy, lister)
}

// embeddingModelLister is the slice of ModelDiscovery collectEmbeddingServices
// needs. Narrow interface + injected running/health checks so the SELECTION and
// ATTRIBUTION logic is unit-testable without docker, a native process, or a bound
// port, the same pattern discoverLocalEngines uses.
type embeddingModelLister interface {
	DiscoverEmbeddingModel(ctx context.Context, port int) ([]string, error)
}

// collectEmbeddingServices is collectEmbeddingServiceStatus with its live probes
// injected.
//
// Readiness rides Health, not presence (aceteam#7148). A container that is up but
// whose model is still downloading answers /health with a non-200 and used to be
// omitted from the heartbeat entirely, so a node that had just been told to start
// TEI reported "no services" for the whole warm-up while `service_status` said it
// was running. It is now reported Status=running with Health=starting and no
// models: a warming server has none to name, and naming one anyway would be
// citadel-cli#690 in reverse. The platform gates embedding dispatch on
// Health=ok, so a warming node is still never handed an embed that would 503.
func collectEmbeddingServices(
	ctx context.Context,
	portIfRunning func(name string) (int, bool),
	healthy func(port int) bool,
	md embeddingModelLister,
) []ServiceInfo {
	var out []ServiceInfo
	for _, name := range embeddingProbeServices {
		port, running := portIfRunning(name)
		if !running || port <= 0 {
			continue
		}
		info := ServiceInfo{
			Name:   name,
			Type:   ServiceTypeEmbedding,
			Status: ServiceStatusRunning,
			Port:   port,
			Health: HealthStatusStarting,
		}
		// TEI answers /health with 200 only once the model has loaded, so this
		// is the ready-vs-warming decision.
		if healthy(port) {
			info.Health = HealthStatusOK
			if md != nil {
				mctx, cancel := context.WithTimeout(ctx, ModelDiscoveryTimeout)
				models, err := md.DiscoverEmbeddingModel(mctx, port)
				cancel()
				if err == nil {
					info.Models = models
				}
			}
		}
		out = append(out, info)
	}
	return out
}

// embeddingServiceHealthy reports whether the embedding server on the loopback
// host port answers TEI's /health endpoint with 200 (returned only after the
// model has loaded).
func embeddingServiceHealthy(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// engineInList reports whether name is present in the given engine list.
func engineInList(list []string, name string) bool {
	for _, e := range list {
		if e == name {
			return true
		}
	}
	return false
}

// managedEnginePortIfRunning reports whether a managed engine is running and,
// if so, its host port. It checks the container "citadel-<name>" first (the
// compose deploy path) using the given engine binary (docker or podman), and
// falls back to the native process check. The host port is resolved from the
// embedded compose file's port mapping, falling back to the known native
// default.
func managedEnginePortIfRunning(engineBin, name string) (port int, running bool) {
	return enginePortIfRunning(runningEmbeddedServices(engineBin), name)
}

// enginePortIfRunning is managedEnginePortIfRunning against an already-collected
// running-container set, so one heartbeat pass makes ONE container-runtime call
// instead of one per engine.
func enginePortIfRunning(running map[string]bool, name string) (port int, isRunning bool) {
	if running[name] {
		return managedEngineHostPort(name), true
	}
	// Serving, not process-present (#649). This function decides what the node
	// tells the fabric it can serve, so a `pgrep` match on a dead engine here
	// became "keep routing inference to this node" -- every request then timed
	// out. IsNativeServiceServing asks the only question routing depends on.
	if nativesvc.IsNativeServiceServing(name) {
		if p, ok := nativesvc.GetServicePort(name); ok {
			return p, true
		}
		return managedEngineHostPort(name), true
	}
	return 0, false
}

// composePortRe matches the host side of a compose short-form port mapping,
// e.g. "8100:8000" or "127.0.0.1:8100:8000" -> host port 8100.
var composePortRe = regexp.MustCompile(`(?:\d+\.\d+\.\d+\.\d+:)?(\d+):\d+`)

// managedEngineHostPort resolves the published host port for a managed engine.
// For engines whose host publish citadel owns via ${CITADEL_*_HOST_PORT}
// substitution (llamacpp/vllm/extraction/diffusers), the compose file no longer
// carries a literal host port, so the port comes from the registry
// (services/ports.go). For any other engine it falls back to parsing the first
// port mapping of its embedded compose file. Returns 0 when neither yields a
// port.
func managedEngineHostPort(name string) int {
	if port, ok := services.ManagedServiceHostPort(name); ok {
		return port
	}
	compose, ok := services.ServiceMap[name]
	if !ok {
		return 0
	}
	return firstComposeHostPort(compose)
}

// firstComposeHostPort parses a compose document and returns the host port of
// the first service's first port mapping, or 0 if none is found.
func firstComposeHostPort(composeYAML string) int {
	var doc struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeYAML), &doc); err != nil {
		return 0
	}
	for _, svc := range doc.Services {
		for _, p := range svc.Ports {
			if m := composePortRe.FindStringSubmatch(p); m != nil {
				if hp, err := strconv.Atoi(m[1]); err == nil {
					return hp
				}
			}
		}
	}
	return 0
}
