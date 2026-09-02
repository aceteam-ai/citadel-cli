package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// chat_route.go implements the node-side of served-engine chat routing
// (issue #581), the complement of the backend served-engine routing
// (aceteam #6236).
//
// WHY this exists: on embedded-tsnet nodes an engine's host port is NOT
// reachable over the mesh — only ports citadel explicitly ListenVPN's (status,
// gateway, terminal, vnc, modules) answer. So a peer that discovers this node
// serves a model (via GET /status, internal/mesh) cannot dial the engine's
// :8210/:8100 directly; it must go through the gateway. Before this, the gateway
// proxied only /v1/embeddings (a single static upstream) and control routes, so
// mesh-direct chat had no reachable endpoint (mesh chat failed gracefully,
// pointing here).
//
// WHAT this adds: /v1/chat/completions, /v1/completions, and /v1/models on the
// gateway mux, so they ride the same LAN + VPN listener as every other gateway
// route. Chat/completions and completions resolve the requested model to the
// LOCAL engine serving it (vllm/llamacpp/ollama/bonsai and their citadel-owned
// host ports) and reverse-proxy to that engine's OpenAI-compatible endpoint,
// streaming SSE included. Unlike the static Upstream map (one fixed address per
// path), the backend here is chosen per request from the body's "model", so a
// multi-engine node (e.g. vllm + bonsai) routes by model rather than to a single
// upstream.

// maxChatProbeBody bounds how much of a chat request body the router buffers to
// read the "model" field. The full buffered body is still forwarded verbatim;
// this only caps the peek so a hostile/buggy client cannot make the router hold
// an unbounded body in memory.
const maxChatProbeBody = 8 << 20 // 8 MiB

// ChatUpstream is one local serving engine on THIS node that can answer
// OpenAI-compatible chat/completions, plus the model(s) it currently serves.
// Engine is informational (vllm/llamacpp/ollama/bonsai); Port is the
// citadel-owned host port the engine's OpenAI-compatible API listens on
// (localhost); Models are the loaded model id(s). It mirrors
// status.LocalEngine, kept as a local type so the gateway package stays free of
// the heavy internal/status transitive deps and the routing logic is
// unit-testable with a hand-built lister.
type ChatUpstream struct {
	Engine string
	Port   int
	Models []string
}

// ChatModelLister returns the local serving engines and their models. The
// gateway calls it per request (cmd wires a short-TTL-cached
// status.DiscoverLocalEngines) so routing reflects live state — an engine that
// loads or unloads a model changes where subsequent chat requests route.
type ChatModelLister func() []ChatUpstream

// SetChatRouter enables model->engine chat routing on the gateway. When set,
// Start registers /v1/chat/completions, /v1/completions, and /v1/models. Passing
// nil leaves those routes unregistered (the pre-#581 behavior). Must be called
// before Start.
func (s *Server) SetChatRouter(lister ChatModelLister) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatLister = lister
}

// SetRequestRecorder wires a callback that records a node-routed request
// against the resolved engine name (citadel #691). Passing nil (the default)
// disables recording. Must be called before Start, like SetChatRouter.
func (s *Server) SetRequestRecorder(recorder func(engine string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestRecorder = recorder
}

// SwapOutcome mirrors the fields of worker.SwapOutcome the gateway needs to
// shape a model_warming HTTP response (citadel-cli#686) — defined locally,
// with gateway-owned types, for the same reason ModelSwapper is: this package
// must not import internal/worker (see the ChatUpstream doc comment above).
type SwapOutcome struct {
	// Ready is true when the target engine is resident and serving; the
	// caller then proceeds to the normal proxy path.
	Ready bool
	// ETASeconds is the estimated remaining seconds until the model is ready.
	ETASeconds int
	// RetryAfterSeconds is the hint a caller should wait before retrying. <= 0
	// means "use the standard hint" (see warmingRetryAfter).
	RetryAfterSeconds int
	// WarmingFor names the model actually loading on this node right now,
	// which may differ from the requested model when a DIFFERENT swap is
	// holding the single-flight slot (worker.SwapOutcome.WarmingFor,
	// citadel-cli#681). Empty when unknown.
	WarmingFor string
}

// ModelSwapper is the minimal interface the gateway chat route needs to make an
// installed-but-not-currently-resident model available before routing a request
// to it (citadel-cli#686). It is the SAME operation the job path already uses
// (worker.SwapManager.EnsureResident) — defined locally, with gateway-owned
// types, rather than by importing internal/worker, so this package keeps the
// import direction it already has everywhere else (gateway depends on nothing
// under internal/worker; cmd/, which already imports both, supplies the real
// adapter via SetModelSwapper).
type ModelSwapper interface {
	EnsureResident(ctx context.Context, backend, model string) (SwapOutcome, error)
}

// SetModelSwapper wires the node's model-hotswap manager to the gateway
// (citadel-cli#686). A *worker.SwapManager is constructed in
// cmd/work.go's buildNodeJobHandlers, which happens AFTER the gateway's chat
// router is wired (SetChatRouter) and after gw.Start has already been kicked
// off in its own goroutine (cmd/work.go's runWork) — so there is no point in
// the startup sequence at which a caller could hand the gateway a valid
// swapper reference directly. SetModelSwapper is called at
// gateway-construction time in cmd/, wrapping the SAME
// atomic.Pointer[worker.SwapManager] the heartbeat's swap-stats reporting
// already reads (see cmd/work.go's nodeSwapManager), so the adapter resolves
// to the real manager the moment buildNodeJobHandlers populates that pointer,
// with no reordering of the existing startup sequence required.
//
// resolveWithFallback (below) is what actually calls EnsureResident, on a
// chatLister miss, only when installedLister also names the model as
// installed-but-stopped. Safe to call at any time (mutex-guarded, like
// SetChatRouter/SetRequestRecorder above); passing nil disables the fallback
// entirely (`citadel serve`, which constructs no SwapManager, never calls
// this — the fallback is then simply never consulted, unchanged from before
// this existed).
func (s *Server) SetModelSwapper(swapper ModelSwapper) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelSwapper = swapper
}

// SetInstalledModelLister wires the installed-but-stopped engine lister
// (citadel-cli#686). Consulted only on a chatLister (running-engine) miss,
// and only when a ModelSwapper is also wired — without one, a match here
// could never actually be brought up, so there is nothing useful to do with
// it. Must be called before Start, like SetChatRouter. Passing nil disables
// the fallback (the default): a running-engine miss then goes straight to
// the existing 404 model_not_found, unchanged from before this existed.
func (s *Server) SetInstalledModelLister(lister ChatModelLister) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installedLister = lister
}

// registerChatRoutes wires the chat-routing handlers onto the mux. It is called
// from Start (and directly from tests) so the test exercises the SAME
// registration path as production rather than a hand-rolled parallel mux.
func (s *Server) registerChatRoutes() {
	chat := http.HandlerFunc(s.handleChatCompletions)
	// Both paths route identically — resolve the model, proxy to its engine. The
	// engines expose /v1/chat/completions and /v1/completions at the same
	// (ip,port), so the router forwards the path unchanged.
	s.mux.Handle("/v1/chat/completions", chat)
	s.mux.Handle("/v1/completions", chat)
	// /v1/models aggregates the models served locally (no model in the request,
	// so it is a plain listing rather than a routed proxy).
	s.mux.Handle("/v1/models", http.HandlerFunc(s.handleModels))
}

// handleChatCompletions reads the requested model from the body, resolves it to
// a local engine's host port, and reverse-proxies the request (verbatim body,
// streaming SSE included) to that engine's OpenAI-compatible endpoint. An
// unknown model yields a 404 with an OpenAI-shaped error.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lister := s.chatLister
	installedLister := s.installedLister
	swapper := s.modelSwapper
	recorder := s.requestRecorder
	nodeName := s.config.NodeName
	s.mu.RUnlock()

	if lister == nil {
		writeChatError(w, http.StatusNotFound, "model_not_found", "chat routing not enabled on this node")
		return
	}

	// Buffer the body so we can read "model" and then forward it VERBATIM. The
	// body is forwarded byte-for-byte (never re-marshalled from a parsed struct)
	// so the metering middleware's stream_options.include_usage injection — and
	// every client field — survives to the engine.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxChatProbeBody))
	_ = r.Body.Close()
	if err != nil {
		writeChatError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var probe struct {
		Model string `json:"model"`
	}
	// A malformed body still reaches the engine (which returns its own 4xx); we
	// only need "model" to pick the route, so an unmarshal error is non-fatal.
	_ = json.Unmarshal(body, &probe)
	model := strings.TrimSpace(probe.Model)

	port, engine, ok, wrote := s.resolveWithFallback(r.Context(), w, model, lister(), installedLister, swapper)
	if wrote {
		// resolveWithFallback already wrote a 503 (model_warming or a swap
		// error) directly to w; nothing more to do.
		return
	}
	if !ok {
		writeChatError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("model %q not served on this node", model))
		return
	}

	// Record the dispatch against the resolved engine (citadel #691) before
	// proxying. This fires on every request routed here regardless of the
	// upstream engine's own response, matching the vLLM idle tracker's "active"
	// gauge semantics (a queued/in-flight request already counts as activity) --
	// recording only on a verified 2xx would need wrapping the ResponseWriter
	// for no real benefit, since a request that reached here already proves the
	// node is routing traffic to this engine.
	if recorder != nil {
		recorder(engine)
	}

	// Restore the body for the proxy.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	// Dial the engine on the loopback host port. 127.0.0.1 (not "localhost") to
	// dodge an IPv6-first (::1) resolution against an IPv4-only engine bind.
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// Path (/v1/chat/completions or /v1/completions) is forwarded
			// unchanged — the engine serves the identical path.
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
			req.Header.Set("X-Forwarded-Proto", "https")
			if nodeName != "" {
				req.Header.Set("X-Citadel-Node", nodeName)
			}
		},
		// -1 flushes each write immediately so streaming (stream:true) SSE chunks
		// reach the client as they arrive instead of being buffered.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[Gateway] chat proxy error for %s -> %s (engine=%s): %v", r.URL.Path, target.Host, engine, err)
			writeChatError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("engine %q unavailable", engine))
		},
	}
	proxy.ServeHTTP(w, r)
}

// warmingRetryAfter is the standard Retry-After hint (seconds) when a swap
// outcome does not name a better one. Mirrors worker.warmingRetryAfter (the
// job path's identical constant) so a caller sees the same pacing whether it
// hit the gateway or the job/stream contract.
const warmingRetryAfter = 10

// resolveWithFallback resolves model to a routable (port, engine), consulting
// the installed-but-stopped fallback (citadel-cli#686) on a running-engine
// (lister) miss: this is the fix for "one node answers the same question two
// ways" — before this, an installed-but-stopped engine 404'd here even though
// the SAME node would happily swap it in and serve it via the worker's job
// path.
//
// Return contract (mirrors the shape callers of a "did I already handle this"
// helper expect, since there are three outcomes, not two):
//   - ok=true: route to (port, engine) normally. wrote is always false here.
//   - ok=false, wrote=true: this function already wrote the HTTP response (a
//     503 model_warming, or a 503 swap-error) directly to w. The caller must
//     return without writing anything else.
//   - ok=false, wrote=false: no running engine and no installed-but-stopped
//     match either (or no installedLister/swapper wired). The caller falls
//     through to its own 404 model_not_found — unchanged from before this
//     fallback existed.
//
// EnsureResident is called with the request's own context, and itself blocks
// only up to the swap manager's wait budget (worker.swapWaitBudget, currently
// 15s) before returning Ready=false rather than the full model load — the
// SAME bound the job path's identical call already accepts, so this does not
// newly risk holding the HTTP request open for a multi-minute cold start.
//
// Known narrow gap, not fixed here (would mean touching swap plumbing, out of
// #686's scope): worker.SwapManager.EnsureResident's FAST path (the target
// already resident when called) reports Ready=true from a bare
// SwapController.Resident check -- "the container is up", NOT "the engine is
// serving" (see worker/llm_inference.go's own doc comment on this exact
// distinction, citadel-cli#680). The job path re-probes with
// ensureEngineReady AFTER its swapper block specifically to cover this; this
// gateway path does not have an equivalent engine-agnostic readiness probe
// available to it (internal/gateway deliberately has no internal/status
// dependency) and instead relies on the reverse proxy's own ErrorHandler
// (502 upstream_error) if the engine is not yet actually accepting
// connections. In practice this fast path is reached only when the container
// is already up but was NOT found by the running-engine lister's own
// DiscoverModels probe (chatLister only lists engines that already answer),
// so the window is narrow: a container that just started and has not yet
// bound its port. A slower-but-honest swap (the non-fast path) already
// blocks on a REAL SwapController.Ready probe before reporting Ready=true.
func (s *Server) resolveWithFallback(
	ctx context.Context,
	w http.ResponseWriter,
	model string,
	running []ChatUpstream,
	installedLister ChatModelLister,
	swapper ModelSwapper,
) (port int, engine string, ok, wrote bool) {
	if port, engine, ok = resolveChatModel(model, running); ok {
		return port, engine, true, false
	}
	// An empty model must never trigger the installed fallback: resolveChatModel
	// treats an empty model as "route unambiguously to the only candidate",
	// which is a reasonable convenience for an ALREADY-RUNNING single-engine
	// node (nothing changes) but would be a hazard here -- a request that
	// simply omitted "model" could otherwise induce a real eviction+swap
	// against a node with exactly one installed-but-stopped engine. Require an
	// explicit model name before ever calling EnsureResident.
	if model == "" || installedLister == nil || swapper == nil {
		return 0, "", false, false
	}
	instPort, instEngine, instOK := resolveChatModel(model, installedLister())
	if !instOK {
		return 0, "", false, false
	}
	outcome, err := swapper.EnsureResident(ctx, instEngine, model)
	if err != nil {
		// A node refusing the swap (rate-limited, preflight-blocked, ...) or a
		// hard failure either way — not "coming soon", so this is a 503
		// upstream_error rather than model_warming, matching the job path's
		// unavailable()/failure() split for the same distinction.
		writeChatError(w, http.StatusServiceUnavailable, "upstream_error",
			fmt.Sprintf("model %q could not be brought up on this node: %v", model, err))
		return 0, "", false, true
	}
	if !outcome.Ready {
		writeChatWarming(w, model, outcome.ETASeconds, outcome.RetryAfterSeconds, outcome.WarmingFor)
		return 0, "", false, true
	}
	return instPort, instEngine, true, false
}

// writeChatWarming writes a 503 signaling the model is being swapped in but
// not yet ready, mirroring the worker job path's model_warming contract
// (LLMInferenceHandler.warming, internal/worker/llm_inference.go) so a caller
// retrying after retry_after seconds is speaking a shape it already knows how
// to parse, just carried over HTTP instead of the job/stream contract:
// top-level status/model/eta_seconds/retry_after(/warming_for), alongside an
// OpenAI-shaped error object for a plain OpenAI-client caller. The standard
// HTTP Retry-After header (RFC 7231) is set too, so a generic HTTP client
// backs off correctly even without model_warming-aware parsing.
func writeChatWarming(w http.ResponseWriter, model string, etaSeconds, retryAfterSeconds int, warmingFor string) {
	if etaSeconds < 0 {
		etaSeconds = 0
	}
	if retryAfterSeconds <= 0 {
		retryAfterSeconds = warmingRetryAfter
	}
	body := map[string]any{
		"error": map[string]string{
			"message": fmt.Sprintf("model %q is warming up on this node", model),
			"type":    "model_warming",
		},
		"status":      "model_warming",
		"model":       model,
		"eta_seconds": etaSeconds,
		"retry_after": retryAfterSeconds,
	}
	if warmingFor != "" {
		body["warming_for"] = warmingFor
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(body)
}

// handleModels returns the OpenAI-compatible /v1/models listing aggregated from
// the local serving engines. Duplicate model ids (same model on two engines) are
// de-duplicated; the first engine wins the owned_by field.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	lister := s.chatLister
	s.mu.RUnlock()

	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := []modelObj{}
	if lister != nil {
		seen := map[string]bool{}
		for _, e := range lister() {
			for _, m := range e.Models {
				m = strings.TrimSpace(m)
				if m == "" || seen[m] {
					continue
				}
				seen[m] = true
				data = append(data, modelObj{ID: m, Object: "model", OwnedBy: e.Engine})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// resolveChatModel picks the local engine host port that serves the requested
// model. Matching mirrors internal/mesh.FindModel (the discovery-side selector)
// so the gateway agrees with what a peer's `citadel mesh chat` discovered:
//
//   - exact, case-insensitive model-id match first (the load-bearing case —
//     `mesh chat` forwards the exact discovered model id from the peer's
//     /status), then
//   - a case-insensitive substring match as a fallback (a human hitting the
//     gateway with a short alias).
//
// Ordering is deterministic (sorted by model, then engine, then port) so a
// substring that matches multiple engines resolves to a stable pick rather than
// map-iteration-random. An empty model routes only when unambiguous — every
// candidate resolves to the same port (a single engine); otherwise it is a miss
// so the caller returns 404 rather than guessing. Returns ok=false when nothing
// serves the model.
//
// ResolveChatModel exports this for callers outside a gateway HTTP request --
// e.g. `citadel mcp`'s local inference tool (aceteam #8249), which routes a
// chat request directly to 127.0.0.1:<port> without going through the gateway
// mux at all, but must still agree with the gateway on which engine serves a
// given model. Keeping ONE resolver (this one) means that agreement can never
// drift; ResolveChatModel is a zero-logic wrapper, never a second
// implementation.
func ResolveChatModel(model string, engines []ChatUpstream) (port int, engine string, ok bool) {
	return resolveChatModel(model, engines)
}

func resolveChatModel(model string, engines []ChatUpstream) (port int, engine string, ok bool) {
	type cand struct {
		engine string
		port   int
		model  string
	}
	var all []cand
	for _, e := range engines {
		if e.Port <= 0 {
			continue
		}
		if len(e.Models) == 0 {
			// A running engine with no discovered model can still serve the
			// empty-model case (a single-engine node), so record it with an empty
			// model id.
			all = append(all, cand{engine: e.Engine, port: e.Port})
			continue
		}
		for _, m := range e.Models {
			all = append(all, cand{engine: e.Engine, port: e.Port, model: strings.TrimSpace(m)})
		}
	}
	if len(all) == 0 {
		return 0, "", false
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].model != all[j].model {
			return all[i].model < all[j].model
		}
		if all[i].engine != all[j].engine {
			return all[i].engine < all[j].engine
		}
		return all[i].port < all[j].port
	})

	model = strings.TrimSpace(model)
	if model == "" {
		// Route only when unambiguous: all candidates on one port (one engine).
		firstPort := all[0].port
		for _, c := range all {
			if c.port != firstPort {
				return 0, "", false
			}
		}
		return all[0].port, all[0].engine, true
	}

	// Exact, case-insensitive id match.
	for _, c := range all {
		if c.model != "" && strings.EqualFold(c.model, model) {
			return c.port, c.engine, true
		}
	}
	// Substring fallback; deterministic first match (all is sorted).
	needle := strings.ToLower(model)
	for _, c := range all {
		if c.model != "" && strings.Contains(strings.ToLower(c.model), needle) {
			return c.port, c.engine, true
		}
	}
	return 0, "", false
}

// writeChatError writes an OpenAI-shaped error object with the given HTTP status.
func writeChatError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": typ},
	})
}
