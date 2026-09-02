package status

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/engine"
	nativesvc "github.com/aceteam-ai/citadel-cli/internal/services"
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
//
// citadel #685 slice 2: this is now a copy of internal/engine's own
// idleCapableEngineNames, not a second hand-maintained literal -- extending
// the idle signal to another engine is now a one-line change in
// internal/engine/tables.go, read here at package init.
var idleCapableEngines = engine.IdleCapableEngineNames()

// managedEngineHostPort resolves the published host port for a managed
// engine or any other citadel-owned embedded service by name. citadel #685
// slice 2: delegates to internal/engine.HostPortForName, the single
// implementation of the registry-first-then-compose-parse resolution this
// function used to own directly (see internal/engine/tables.go's doc comment
// for why that function is a GENERAL resolver, not scoped to
// services.ServiceMap/the Registry).
func managedEngineHostPort(name string) int {
	return engine.HostPortForName(name)
}

// ManagedEngineHostPort exposes managedEngineHostPort to callers outside this
// package that need the exact host port this package's own heartbeat/status
// collection would resolve for a serving engine.
func ManagedEngineHostPort(name string) int {
	return managedEngineHostPort(name)
}

// ManagedProbeEngines returns a copy of managedProbeEngines, the list of
// engines the heartbeat's model/health probe iterates.
func ManagedProbeEngines() []string {
	out := make([]string, len(managedProbeEngines))
	copy(out, managedProbeEngines)
	return out
}

// IdleCapableEngines returns a copy of idleCapableEngines -- the engines with a
// reliable SCRAPED idle/request signal.
func IdleCapableEngines() []string {
	out := make([]string, len(idleCapableEngines))
	copy(out, idleCapableEngines)
	return out
}

// EmbeddingProbeServices returns a copy of embeddingProbeServices.
func EmbeddingProbeServices() []string {
	out := make([]string, len(embeddingProbeServices))
	copy(out, embeddingProbeServices)
	return out
}

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
//
// sglang (citadel-cli#685 §1b) was missing from this list even though it has a
// full dispatch path elsewhere (llm_inference.go's executeSGLang, an
// engineReadyPath entry, load/VRAM estimates): that ONE omission made it
// invisible to EngineTypeFromName's gate (models.go) and therefore to
// DiscoverModels, CheckServiceHealth, the gateway chat router, mesh discovery,
// and hotswap residency/preemption tracking all at once — five to six
// consumers from one root cause. Adding a new engine here requires the
// matching DiscoverModels/CheckServiceHealth cases (now internal/engine's
// Dialect field, see models.go) to be set in the same change, or the probe
// added here just errors instead of resolving.
//
// citadel #685 slice 2: a copy of internal/engine's own
// managedProbeEngineNames, not a second hand-maintained literal. Order is
// preserved from the pre-migration literal (see tables.go's doc comment for
// why order is load-bearing here).
var managedProbeEngines = engine.ManagedProbeEngineNames()

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
// Readiness/ProbedAt/Reason (citadel-cli#684) are additive on top of the above:
// Status and Health keep exactly the values described in the paragraph above,
// unchanged. Readiness carries the same "answered vs. timed out" distinction as
// a dedicated four-valued field (ReadinessReady / ReadinessStarting) plus WHEN
// the probe ran and WHY it landed where it did, so a platform that ignores
// Readiness entirely sees byte-identical Status/Health output to before #684.
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
		probeAttempted := c.idleTracker != nil || c.modelDiscovery != nil

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

		// Readiness (citadel-cli#684) is purely additive: Status/Health above are
		// unchanged from pre-#684 behavior. Container is confirmed up (isRunning,
		// from docker ps), so the only question a live probe answers is whether it
		// is serving yet.
		info.Readiness, info.Reason, info.ProbedAt = readinessForProbe(responded, probeAttempted)
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
//
// citadel #685 slice 2: a copy of internal/engine's own
// embeddingCapableEngineNames, not a second hand-maintained literal.
var embeddingProbeServices = engine.EmbeddingCapableEngineNames()

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
		probedAt := time.Now()
		info.ProbedAt = &probedAt
		// TEI answers /health with 200 only once the model has loaded, so this
		// is the ready-vs-warming decision.
		if healthy(port) {
			info.Health = HealthStatusOK
			info.Readiness = ReadinessReady
			if md != nil {
				mctx, cancel := context.WithTimeout(ctx, ModelDiscoveryTimeout)
				models, err := md.DiscoverEmbeddingModel(mctx, port)
				cancel()
				if err == nil {
					info.Models = models
				}
			}
		} else {
			// Readiness (citadel-cli#684) is additive: Health above is unchanged
			// from pre-#684 behavior (still "starting").
			info.Readiness = ReadinessStarting
			info.Reason = "container running, /health has not returned 200 yet"
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

// readinessForProbe derives the additive Readiness/Reason/ProbedAt triple
// (citadel-cli#684) for an engine confirmed running (container up, via
// docker ps). It is a pure function -- no I/O, no clock dependency beyond
// time.Now() -- specifically so the down/starting/ready state matrix is
// directly unit-testable without a live probe or an httptest server.
//
// probeAttempted distinguishes "we tried to ask and it didn't answer" from "we
// never asked at all": the latter only happens with a Collector that has
// neither an idle tracker nor a model-discovery client wired (a bare
// zero-value Collector in a test; production's NewCollector always sets both),
// and in that case there is no live probe to timestamp, so ProbedAt stays nil.
// responded means at least one live probe (idle scrape or model discovery)
// answered within its own timeout this cycle.
//
// The unresponded reason deliberately does NOT say "timed out": DiscoverModels
// fails for three distinct causes -- connection refused (nothing bound to the
// port yet, the common case for the first several seconds/minutes of a real
// weights load, e.g. vLLM importing Python before uvicorn binds), a non-200
// response (still loading), or an actual context.DeadlineExceeded -- and only
// the last of those is a timeout. Claiming "timed out" for a refused
// connection would send an operator hunting for a slow engine instead of an
// unbound port, so the reason names the bound the probe ran under, not a
// specific cause.
func readinessForProbe(responded, probeAttempted bool) (readiness, reason string, probedAt *time.Time) {
	if !probeAttempted {
		if responded {
			// Unreachable in practice (responded requires a probe to have run),
			// but handled explicitly rather than assumed impossible.
			return ReadinessReady, "", nil
		}
		return ReadinessStarting, "container running, no readiness probe available", nil
	}
	now := time.Now()
	if responded {
		return ReadinessReady, "", &now
	}
	return ReadinessStarting, fmt.Sprintf("model discovery probe did not return a served model within %s", ModelDiscoveryTimeout), &now
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
