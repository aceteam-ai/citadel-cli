package provision

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Handler exposes the provisioning API over HTTP. It is intended to be
// registered on the status server's mux, gated by VPN-or-auth middleware.
type Handler struct {
	manager *Manager
}

// NewHandler creates a provisioning HTTP handler backed by the given manager.
func NewHandler(manager *Manager) *Handler {
	return &Handler{manager: manager}
}

// RegisterRoutes attaches the provisioning endpoints to the given mux.
// The authMiddleware wraps each handler with VPN-or-auth gating.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMiddleware func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/provision/create", authMiddleware(h.handleCreate))
	mux.HandleFunc("/provision/list", authMiddleware(h.handleList))
	mux.HandleFunc("/provision/", authMiddleware(h.handleByID))
}

func (h *Handler) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	var spec ResourceSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}

	result, err := h.manager.Create(r.Context(), &spec)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	resources := h.manager.List()
	if resources == nil {
		resources = []*Resource{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"resources": resources,
		"count":     len(resources),
	})
}

func (h *Handler) handleByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/provision/")
	if path == "" || path == "/" {
		h.handleList(w, r)
		return
	}

	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "", "status":
		h.handleStatus(w, r, id)
	case "destroy":
		h.handleDestroy(w, r, id)
	case "logs":
		h.handleLogs(w, r, id)
	default:
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown action %q", action))
	}
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	resource, err := h.manager.Status(r.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resource)
}

func (h *Handler) handleDestroy(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if err := h.manager.Destroy(r.Context(), id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "destroyed"})
}

func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	tail := 100
	if v := r.URL.Query().Get("tail"); v != "" {
		fmt.Sscanf(v, "%d", &tail)
	}

	logs, err := h.manager.Logs(r.Context(), id, tail)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"logs": logs})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
