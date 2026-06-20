package workflow

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Server struct {
	executor *Executor
}

func NewServer(executor *Executor) *Server {
	return &Server{executor: executor}
}

// RegisterRoutes registers workflow HTTP endpoints on the given mux.
// The authMiddleware parameter gates all routes behind authentication
// (e.g., requireVPNOrAuth from the status server). Pass nil to skip
// auth gating (only safe in tests).
func (s *Server) RegisterRoutes(mux *http.ServeMux, authMiddleware func(http.HandlerFunc) http.HandlerFunc) {
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		if authMiddleware != nil {
			return authMiddleware(h)
		}
		return h
	}
	mux.HandleFunc("/workflow/run", wrap(s.handleRun))
	mux.HandleFunc("/workflow/", wrap(s.handleWorkflowByID))
	mux.HandleFunc("/workflow", wrap(s.handleList))
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
	exec, err := s.executor.Submit(r.Context(), &req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, RunResponse{ID: exec.ID, Status: exec.Status})
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
