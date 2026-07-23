package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
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
