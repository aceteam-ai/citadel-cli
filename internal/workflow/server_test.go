package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// noopAuth is a pass-through middleware used in tests that don't verify auth gating.
func noopAuth(h http.HandlerFunc) http.HandlerFunc { return h }

func newTestServer() (*Server, *Executor) {
	executor := NewExecutor(ExecutorConfig{DefaultTimeout: 5 * time.Second})
	server := NewServer(executor)
	return server, executor
}

func TestServer_RunEndpoint(t *testing.T) {
	srv, _ := newTestServer()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, noopAuth)
	body := `{"graph":{"input_node":{"id":"in","type":"Input"},"output_node":{"id":"out","type":"Output"},"inner_nodes":[],"edges":[{"source_id":"in","source_key":"data","target_id":"out","target_key":"result"}]},"input":{"data":"test"}}`
	req := httptest.NewRequest(http.MethodPost, "/workflow/run", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp RunResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ID == "" {
		t.Fatal("expected non-empty ID")
	}
}

func TestServer_RunEndpoint_InvalidMethod(t *testing.T) {
	srv, _ := newTestServer()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, noopAuth)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/workflow/run", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestServer_RunEndpoint_InvalidJSON(t *testing.T) {
	srv, _ := newTestServer()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, noopAuth)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/workflow/run", bytes.NewBufferString("not json")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestServer_GetEndpoint(t *testing.T) {
	srv, executor := newTestServer()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, noopAuth)
	graph := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		Edges: []*Edge{{SourceID: "in", SourceKey: "x", TargetID: "out", TargetKey: "y"}},
	}
	exec, _ := executor.Submit(context.Background(), &RunRequest{Graph: graph})
	time.Sleep(200 * time.Millisecond)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/workflow/"+exec.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestServer_GetEndpoint_NotFound(t *testing.T) {
	srv, _ := newTestServer()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, noopAuth)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/workflow/nonexistent", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestServer_ListEndpoint(t *testing.T) {
	srv, executor := newTestServer()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, noopAuth)
	graph := &WorkflowGraph{
		InputNode: &Node{ID: "in", Type: NodeTypeInput}, OutputNode: &Node{ID: "out", Type: NodeTypeOutput},
		Edges: []*Edge{{SourceID: "in", SourceKey: "x", TargetID: "out", TargetKey: "y"}},
	}
	executor.Submit(context.Background(), &RunRequest{Graph: graph})
	time.Sleep(100 * time.Millisecond)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/workflow", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var result map[string]any
	json.Unmarshal(w.Body.Bytes(), &result)
	if result["count"].(float64) != 1 {
		t.Fatal("expected count=1")
	}
}

func TestServer_CancelEndpoint(t *testing.T) {
	srv, executor := newTestServer()
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux, noopAuth)
	executor.mu.Lock()
	executor.runs["cancel-me"] = &Execution{ID: "cancel-me", Status: StatusRunning}
	executor.mu.Unlock()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/workflow/cancel-me", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestServer_RoutesGatedByAuth(t *testing.T) {
	srv, _ := newTestServer()
	mux := http.NewServeMux()
	blocked := func(http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }
	}
	srv.RegisterRoutes(mux, blocked)
	for _, p := range []string{"/workflow/run", "/workflow/abc", "/workflow"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("path %s not gated: got %d", p, w.Code)
		}
	}
}
