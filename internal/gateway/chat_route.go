package gateway

import (
	"bytes"
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

	port, engine, ok := resolveChatModel(model, lister())
	if !ok {
		writeChatError(w, http.StatusNotFound, "model_not_found",
			fmt.Sprintf("model %q not served on this node", model))
		return
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
