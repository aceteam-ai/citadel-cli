package rag

import (
	"encoding/json"
	"io"
	"net/http"
)

// Server exposes the node-local RAG capability over the status server's HTTP
// mux. It mirrors internal/workflow's Server.RegisterRoutes: routes are gated by
// the caller-supplied auth middleware (requireVPNOrAuth), so the backend can
// reach them over the mesh after org validation and a local caller must present
// a token. No new listener/port — it reuses the existing status server.
type Server struct {
	svc *Service
}

// NewServer wraps a Service for HTTP exposure.
func NewServer(svc *Service) *Server { return &Server{svc: svc} }

// RegisterRoutes attaches the /rag/* endpoints to mux behind authMiddleware.
func (s *Server) RegisterRoutes(mux *http.ServeMux, authMiddleware func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/rag/index", authMiddleware(s.handleIndex))
	mux.HandleFunc("/rag/query", authMiddleware(s.handleQuery))
	mux.HandleFunc("/rag/status", authMiddleware(s.handleStatus))
}

type indexRequest struct {
	Path        string `json:"path"`
	FilePattern string `json:"file_pattern"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req indexRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := s.svc.Index(r.Context(), req.Path, req.FilePattern)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type queryRequest struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req queryRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query is required"})
		return
	}
	res, err := s.svc.Query(r.Context(), req.Query, req.TopK)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	st, err := s.svc.Status()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// decodeBody reads and JSON-decodes a request body with a sane size cap.
func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
