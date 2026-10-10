package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nodeindex"
)

// passthroughAuth is a no-op auth middleware for exercising the route wiring;
// the real server injects requireVPNOrAuth.
func passthroughAuth(next http.HandlerFunc) http.HandlerFunc { return next }

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	svc, ws := newTestService(t)
	mux := http.NewServeMux()
	NewServer(svc).RegisterRoutes(mux, passthroughAuth)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, ws
}

func TestServerIndexRequestCancellationIsHard(t *testing.T) {
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"max_client_batch_size":32}`)) })
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	tei := httptest.NewServer(mux)
	defer tei.Close()
	t.Setenv("CITADEL_TEI_URL", tei.URL)
	t.Setenv("CITADEL_INDEX_HNSW", "false")
	ws := t.TempDir()
	path := filepath.Join(ws, "a.md")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	svc := New(ws, "")
	svc.dbPath = dbPath
	body, _ := json.Marshal(indexRequest{Path: ws})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/rag/index", bytes.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { NewServer(svc).handleIndex(rec, req); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("embedding request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler did not propagate request cancellation")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s, want 502", rec.Code, rec.Body.String())
	}
	store, err := nodeindex.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, indexed, err := store.FileHash(path); err != nil || indexed {
		t.Fatalf("row indexed/error=%v/%v, want false/nil", indexed, err)
	}
}

func TestServerIndexQueryStatusRoundTrip(t *testing.T) {
	ts, ws := newTestServer(t)
	writeFile(t, ws, "cats.md", "The cat sat on the mat. A kitten is a small feline.")

	// index
	idxBody, _ := json.Marshal(map[string]string{"path": ws})
	resp, err := http.Post(ts.URL+"/rag/index", "application/json", bytes.NewReader(idxBody))
	if err != nil {
		t.Fatalf("POST /rag/index: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", resp.StatusCode)
	}
	var idx IndexResult
	_ = json.NewDecoder(resp.Body).Decode(&idx)
	resp.Body.Close()
	if idx.FilesIndexed != 1 {
		t.Fatalf("expected 1 file indexed, got %d", idx.FilesIndexed)
	}

	// query
	qBody, _ := json.Marshal(map[string]any{"query": "kitten", "top_k": 3})
	resp, err = http.Post(ts.URL+"/rag/query", "application/json", bytes.NewReader(qBody))
	if err != nil {
		t.Fatalf("POST /rag/query: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query status = %d", resp.StatusCode)
	}
	var qr QueryResult
	_ = json.NewDecoder(resp.Body).Decode(&qr)
	resp.Body.Close()
	if len(qr.Hits) == 0 || filepath.Base(qr.Hits[0].Path) != "cats.md" {
		t.Fatalf("unexpected query hits: %+v", qr.Hits)
	}

	// status
	resp, err = http.Get(ts.URL + "/rag/status")
	if err != nil {
		t.Fatalf("GET /rag/status: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var st Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Files != 1 {
		t.Fatalf("expected 1 file in status, got %d", st.Files)
	}
}

func TestServerRejectsBadInput(t *testing.T) {
	ts, _ := newTestServer(t)

	// empty query -> 400
	qBody, _ := json.Marshal(map[string]any{"query": ""})
	resp, err := http.Post(ts.URL+"/rag/query", "application/json", bytes.NewReader(qBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty query should be 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// wrong method on status -> 405
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/rag/status", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /rag/status should be 405, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
