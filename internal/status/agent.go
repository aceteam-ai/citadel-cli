package status

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// Agent-facing introspection & control endpoints (issue #236).
//
// These endpoints are served by the same status HTTP server that already
// dual-listens on localhost and the tsnet VPN. Crucially, this control path
// does NOT depend on the Redis shell-job queue it is meant to debug: an agent
// can call these even when job consumption is broken (a job dispatched over the
// per-node stream would hang exactly when you need it). The aceteam MCP server
// wraps these as the `citadel_*` agent tools over the VPN mesh.
//
// To keep the status package decoupled from the worker/runner internals, the
// data and actions are supplied as provider callbacks on ServerConfig, wired up
// in cmd/work.go. When a provider is nil the corresponding endpoint returns 503
// (feature not available in this process), so a status server started outside
// the worker (e.g. `citadel serve`) degrades gracefully.

// AgentProviders supplies the data and actions backing the agent endpoints.
// Read providers are wired on every worker; control providers may be nil.
type AgentProviders struct {
	// WorkerStatus returns a JSON-serializable snapshot of the worker's live
	// consume/subscription state (issue #236, the primary debugging tool).
	WorkerStatus func() any

	// NodeInfo returns node identity: headscale/fabric IDs, version, org,
	// tsnet IP, hostname, uptime, connection state.
	NodeInfo func() any

	// Logs returns up to `lines` recent log lines, optionally filtered by
	// minimum level and a substring grep, since the given relative duration.
	Logs func(opts LogQuery) (string, error)

	// Doctor returns a one-shot healthcheck + "why am I not receiving per-node
	// jobs" diagnosis.
	Doctor func() any

	// Config returns the effective config with secrets redacted.
	Config func() any

	// SetLogLevel toggles verbose/debug console logging. verbose=true enables.
	SetLogLevel func(verbose bool) any

	// Resubscribe re-establishes the per-node queue subscription without
	// restarting the process. Returns a result object.
	Resubscribe func() (any, error)

	// WorkerRestart restarts the worker run loop in place. Returns a result.
	WorkerRestart func() (any, error)

	// Expose programs the in-process gateway to serve a local port under
	// /expose/<name>/ with the given visibility, returning the managed URL (and,
	// for visibility=link, a signed token). It lives here because the gateway
	// runs in THIS process: an out-of-process caller (the CLI, the aceteam MCP)
	// cannot reach it any other way (#598).
	Expose func(ExposeSpec) (any, error)

	// Unexpose revokes an exposure by name, the inverse of Expose. Same
	// in-process reasoning: only this process holds the live gateway.
	Unexpose func(name string) (any, error)
}

// ExposeSpec is the /agent/expose request body. It mirrors the EXPOSE_SET job
// payload so the CLI and the MCP verb drive the same contract.
type ExposeSpec struct {
	// Name is the exposed-service slug (the <name> in /expose/<name>/).
	Name string `json:"name"`
	// Port is the service's loopback host port (e.g. 8212 for the nvr module's
	// Frigate UI).
	Port int `json:"port"`
	// Visibility is "private", "org", or "link".
	Visibility string `json:"visibility"`
	// TTLSeconds bounds a `link` token's lifetime; ignored for private/org.
	TTLSeconds int `json:"ttl_seconds"`
	// Creator is the tailnet login authorized for a `private` exposure. Only a
	// remote caller (the backend/MCP) knows this; a local CLI leaves it empty,
	// which makes a private exposure fail closed at the gateway.
	Creator string `json:"creator"`
	// Epoch, when >0, is bound into a `link` token so all outstanding tokens can
	// be revoked by bumping it.
	Epoch int `json:"epoch"`
}

// LogQuery describes a log tail/grep request.
type LogQuery struct {
	Lines int    // max number of lines (default 200)
	Level string // minimum level filter: "" | info | warning | error
	Grep  string // substring filter
	Since string // relative duration, e.g. "5m", "1h"
}

// registerAgentRoutes attaches the agent introspection & control routes to mux.
// All routes require VPN origin or a valid org token (same posture as the SSH
// key handler): the aceteam backend reaches them over the mesh after validating
// org ownership; local callers must present a token.
func (s *Server) registerAgentRoutes(mux *http.ServeMux) {
	p := s.agent
	if p == nil {
		return
	}

	mux.HandleFunc("/agent/worker-status", s.requireVPNOrAuth(s.handleAgentJSON("worker_status", func(_ *http.Request) (any, error) {
		if p.WorkerStatus == nil {
			return nil, errUnavailable
		}
		return p.WorkerStatus(), nil
	})))

	mux.HandleFunc("/agent/node-info", s.requireVPNOrAuth(s.handleAgentJSON("node_info", func(_ *http.Request) (any, error) {
		if p.NodeInfo == nil {
			return nil, errUnavailable
		}
		return p.NodeInfo(), nil
	})))

	mux.HandleFunc("/agent/doctor", s.requireVPNOrAuth(s.handleAgentJSON("doctor", func(_ *http.Request) (any, error) {
		if p.Doctor == nil {
			return nil, errUnavailable
		}
		return p.Doctor(), nil
	})))

	mux.HandleFunc("/agent/config", s.requireVPNOrAuth(s.handleAgentJSON("config", func(_ *http.Request) (any, error) {
		if p.Config == nil {
			return nil, errUnavailable
		}
		return p.Config(), nil
	})))

	mux.HandleFunc("/agent/logs", s.requireVPNOrAuth(s.handleAgentLogs))

	// Control endpoints (POST, may be nil -> 503).
	mux.HandleFunc("/agent/set-log-level", s.requireVPNOrAuth(s.handleSetLogLevel))
	mux.HandleFunc("/agent/resubscribe", s.requireVPNOrAuth(s.handleAgentAction("resubscribe", func() (any, error) {
		if p.Resubscribe == nil {
			return nil, errUnavailable
		}
		return p.Resubscribe()
	})))
	mux.HandleFunc("/agent/worker-restart", s.requireVPNOrAuth(s.handleAgentAction("worker_restart", func() (any, error) {
		if p.WorkerRestart == nil {
			return nil, errUnavailable
		}
		return p.WorkerRestart()
	})))
	mux.HandleFunc("/agent/expose", s.requireVPNOrAuth(s.handleAgentExpose))
	mux.HandleFunc("/agent/unexpose", s.requireVPNOrAuth(s.handleAgentUnexpose))
}

// handleAgentUnexpose revokes an exposure on the in-process gateway. POST with
// {"name": "..."}; same auth posture as /agent/expose.
//
// POST rather than DELETE to match every other /agent/* control route (they are
// all POST), so the aceteam backend and the CLI need no special-casing here.
func (s *Server) handleAgentUnexpose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.agent == nil || s.agent.Unexpose == nil {
		writeAgentError(w, errUnavailable)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	res, err := s.agent.Unexpose(req.Name)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleAgentExpose programs an exposure on the in-process gateway. POST only,
// with an ExposeSpec body. Same auth posture as the other control endpoints
// (VPN origin or a valid org token), so a local operator drives it via the
// node's own mesh IP and the backend drives it over the mesh.
func (s *Server) handleAgentExpose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.agent == nil || s.agent.Expose == nil {
		writeAgentError(w, errUnavailable)
		return
	}
	var spec ExposeSpec
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	res, err := s.agent.Expose(spec)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// errUnavailable is returned by providers that are not wired in this process.
var errUnavailable = &agentError{code: http.StatusServiceUnavailable, msg: "not available in this process"}

type agentError struct {
	code int
	msg  string
}

func (e *agentError) Error() string { return e.msg }

// handleAgentJSON wraps a read provider into a GET JSON handler.
func (s *Server) handleAgentJSON(name string, fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := fn(r)
		if err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// handleAgentAction wraps a control provider into a POST JSON handler.
func (s *Server) handleAgentAction(name string, fn func() (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		result, err := fn()
		if err != nil {
			writeAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// handleAgentLogs serves GET /agent/logs?lines=&level=&grep=&since=.
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.agent == nil || s.agent.Logs == nil {
		writeAgentError(w, errUnavailable)
		return
	}
	q := LogQuery{
		Lines: 200,
		Level: r.URL.Query().Get("level"),
		Grep:  r.URL.Query().Get("grep"),
		Since: r.URL.Query().Get("since"),
	}
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			q.Lines = n
		}
	}
	out, err := s.agent.Logs(q)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": out})
}

// handleSetLogLevel serves POST /agent/set-log-level?verbose=true|false.
func (s *Server) handleSetLogLevel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.agent == nil || s.agent.SetLogLevel == nil {
		writeAgentError(w, errUnavailable)
		return
	}
	verbose := true
	if v := r.URL.Query().Get("verbose"); v != "" {
		verbose, _ = strconv.ParseBool(v)
	}
	writeJSON(w, http.StatusOK, s.agent.SetLogLevel(verbose))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeAgentError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if ae, ok := err.(*agentError); ok {
		code = ae.code
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
