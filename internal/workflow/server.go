package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

type Server struct {
	executor *Executor
}

func NewServer(executor *Executor) *Server {
	return &Server{executor: executor}
}

// RegisterRoutes installs fail-closed stubs on the plaintext status listener
// (LAN + VPN). No workflow route is served there: mesh origin is not an identity
// (the #5028 rule that moved SSH-key deployment), and the read routes would
// expose a run's graph, inputs, and Shell stdout to any mesh peer, while DELETE
// would let one cancel it. The stubs refuse with a clear signal instead of
// 404ing. The real routes live on the mTLS control listener
// (RegisterControlRoutes).
func (s *Server) RegisterRoutes(mux *http.ServeMux, _ func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/workflow/run", handleMovedToControl)
	mux.HandleFunc("/workflow/", handleMovedToControl)
	mux.HandleFunc("/workflow", handleMovedToControl)
}

// RegisterControlRoutes installs the workflow API on the mTLS control listener,
// every route behind requireCoordinator (a verified fabric-CA coordinator client
// cert). A graph with a Shell node additionally needs the node shell gate (see
// Submit).
func (s *Server) RegisterControlRoutes(mux *http.ServeMux, requireCoordinator func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/workflow/run", requireCoordinator(s.handleRun))
	mux.HandleFunc("/workflow/", requireCoordinator(s.handleWorkflowByID))
	mux.HandleFunc("/workflow", requireCoordinator(s.handleList))
}

func handleMovedToControl(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "the workflow API requires a coordinator mTLS client certificate on the control listener",
	})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return
	}
	var req RunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid JSON: %v", err)})
		return
	}
	// The run outlives this request (we reply 202 immediately), so detach it
	// from the request context, which net/http cancels once the handler returns.
	exec, err := s.executor.Submit(context.WithoutCancel(r.Context()), &req)
	if err != nil {
		var refusal *jobs.ShellRefusal
		if errors.As(err, &refusal) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": refusal.Message, "reason": refusal.Reason})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Read through Snapshot: the run goroutine is already writing Status.
	snap := exec.Snapshot()
	writeJSON(w, http.StatusAccepted, RunResponse{ID: snap.ID, Status: snap.Status})
}

func (s *Server) handleWorkflowByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/workflow/")
	id := strings.TrimSuffix(path, "/")
	if id == "" || id == "run" {
		if id == "" {
			s.handleList(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, id)
	case http.MethodDelete:
		s.handleCancel(w, id)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleGet(w http.ResponseWriter, id string) {
	exec := s.executor.Get(id)
	if exec == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "execution not found"})
		return
	}
	snap := exec.Snapshot()
	writeJSON(w, http.StatusOK, &snap)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	execs := s.executor.List()
	type summary struct {
		ID        string          `json:"id"`
		Status    ExecutionStatus `json:"status"`
		StartedAt string          `json:"started_at"`
	}
	result := make([]summary, 0, len(execs))
	for _, e := range execs {
		snap := e.Snapshot()
		result = append(result, summary{
			ID: snap.ID, Status: snap.Status,
			StartedAt: snap.StartedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"executions": result, "count": len(result)})
}

func (s *Server) handleCancel(w http.ResponseWriter, id string) {
	if s.executor.Cancel(id) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	} else {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "execution not found or not cancellable"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
