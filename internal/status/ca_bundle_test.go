package status

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureFabricCABundleFetchAndCache verifies a successful fetch writes the
// bundle to disk and returns its path, and that the written bundle is a valid
// trust root the verifier can load.
func TestEnsureFabricCABundleFetchAndCache(t *testing.T) {
	ca := newTestCA(t, "fabric-ca")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != caChainPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(ca.certPEM)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path, err := EnsureFabricCABundle(srv.URL, dir)
	if err != nil {
		t.Fatalf("EnsureFabricCABundle: %v", err)
	}
	if path != filepath.Join(dir, fabricCABundleFilename) {
		t.Errorf("path = %q, unexpected", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cached bundle: %v", err)
	}
	if string(got) != string(ca.certPEM) {
		t.Error("cached bundle content does not match fetched chain")
	}
	// The cached bundle must be loadable as a verifier trust root.
	if _, err := NewFabricCAVerifier(path, []string{DefaultCoordinatorSAN}); err != nil {
		t.Errorf("verifier from cached bundle: %v", err)
	}
}

// TestEnsureFabricCABundleReusesCacheOnFetchFailure is the zero-break property:
// once a node has a cached bundle, a later fetch failure (backend down) reuses
// the cache instead of disabling SSH-deploy.
func TestEnsureFabricCABundleReusesCacheOnFetchFailure(t *testing.T) {
	ca := newTestCA(t, "fabric-ca")
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, fabricCABundleFilename)
	if err := os.WriteFile(bundlePath, ca.certPEM, 0644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Point at a closed server so the fetch fails.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	badURL := closed.URL
	closed.Close()

	path, err := EnsureFabricCABundle(badURL, dir)
	if err != nil {
		t.Fatalf("expected cache reuse, got error: %v", err)
	}
	if path != bundlePath {
		t.Errorf("path = %q, want cached %q", path, bundlePath)
	}
}

// TestEnsureFabricCABundleColdStartOffline: no cache + unreachable backend must
// fail (caller then leaves the control listener disabled -- fail closed).
func TestEnsureFabricCABundleColdStartOffline(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	badURL := closed.URL
	closed.Close()

	if _, err := EnsureFabricCABundle(badURL, t.TempDir()); err == nil {
		t.Fatal("expected error on cold start with no cache and unreachable backend")
	}
}

// TestEnsureFabricCABundleBadResponseDoesNotClobberCache: a garbage/non-PEM
// response must not overwrite a good cached bundle.
func TestEnsureFabricCABundleBadResponseDoesNotClobberCache(t *testing.T) {
	ca := newTestCA(t, "fabric-ca")
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, fabricCABundleFilename)
	if err := os.WriteFile(bundlePath, ca.certPEM, 0644); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>gateway error</html>"))
	}))
	defer srv.Close()

	path, err := EnsureFabricCABundle(srv.URL, dir)
	if err != nil {
		t.Fatalf("expected cache reuse on bad response, got: %v", err)
	}
	if path != bundlePath {
		t.Errorf("path = %q, want cached %q", path, bundlePath)
	}
	got, _ := os.ReadFile(bundlePath)
	if string(got) != string(ca.certPEM) {
		t.Error("bad response must not clobber the good cached bundle")
	}
}

// TestEnsureFabricCABundleNon200ReturnsErrorWhenNoCache: a non-200 with no cache
// is an error.
func TestEnsureFabricCABundleNon200ReturnsErrorWhenNoCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := EnsureFabricCABundle(srv.URL, t.TempDir()); err == nil {
		t.Fatal("expected error on 503 with no cache")
	}
}
