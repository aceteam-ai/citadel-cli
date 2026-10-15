package rag

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The plaintext status listener trusts mesh origin, so the RAG API (which reads
// node content) must not be served there at all, even behind a permissive auth
// middleware (citadel-cli#1318). A refused /rag/index must index nothing.
func TestPlaintextServesNoRAGRoutes(t *testing.T) {
	svc, ws := newTestService(t)
	if err := os.WriteFile(filepath.Join(ws, "secret.txt"), []byte("node content"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewServer(svc).RegisterRoutes(mux, passthroughAuth)

	for _, rq := range []struct{ method, path, body string }{
		{http.MethodPost, "/rag/index", `{"path":"` + ws + `"}`},
		{http.MethodPost, "/rag/query", `{"query":"node content"}`},
		{http.MethodGet, "/rag/status", ""},
		{http.MethodGet, "/rag", ""},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(rq.method, rq.path, bytes.NewBufferString(rq.body)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s on the plaintext listener: want 403, got %d: %s", rq.method, rq.path, w.Code, w.Body.String())
		}
		if bytes.Contains(w.Body.Bytes(), []byte("node content")) {
			t.Fatalf("%s %s leaked node content: %s", rq.method, rq.path, w.Body.String())
		}
	}
	st, err := svc.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 0 || st.Chunks != 0 {
		t.Fatalf("a refused plaintext /rag/index still indexed: files=%d chunks=%d", st.Files, st.Chunks)
	}
}

// Every control route is wrapped by the coordinator gate it is handed.
func TestControlRAGRoutesGatedByCoordinator(t *testing.T) {
	svc, ws := newTestService(t)
	mux := http.NewServeMux()
	refuse := func(http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }
	}
	NewServer(svc).RegisterControlRoutes(mux, refuse)
	for _, p := range []string{"/rag/index", "/rag/query", "/rag/status"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, p, bytes.NewBufferString(`{"path":"`+ws+`"}`)))
		if w.Code != http.StatusForbidden {
			t.Fatalf("control %s not gated: got %d", p, w.Code)
		}
	}
}
