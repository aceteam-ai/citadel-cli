package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/services"
)

// These tests use only synthetic temporary cache trees and never touch the
// network — the whole point of the serving contract is that a read is answered
// from local state, with no upstream metadata request.

const (
	testCommit       = "a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2a1b2"
	testEtagConfig   = "cccccccccccccccccccccccccccccccccccccccc"
	testEtagModel    = "dddddddddddddddddddddddddddddddddddddddd"
	testDigestConfig = "1111111111111111111111111111111111111111111111111111111111111111"
	testDigestLayer  = "2222222222222222222222222222222222222222222222222222222222222222"
	testDigestUnref  = "3333333333333333333333333333333333333333333333333333333333333333"
	testPasscode     = "pin-123"
)

// buildCacheFixture materializes a synthetic cache tree (HF hub, GGUF, ollama,
// plus an unsupported native aggregate row) under t.TempDir() and returns a
// provider over an in-memory index describing it, along with the cache root.
func buildCacheFixture(t *testing.T) (CacheIndexProvider, string) {
	t.Helper()
	cacheRoot := t.TempDir()

	// --- HF hub entry: models--org--repo -------------------------------------
	hubEntry := filepath.Join(cacheRoot, "huggingface", "hub", "models--org--repo")
	mustMkdir(t, filepath.Join(hubEntry, "refs"))
	mustMkdir(t, filepath.Join(hubEntry, "blobs"))
	mustMkdir(t, filepath.Join(hubEntry, "snapshots", testCommit, "subdir"))
	mustWrite(t, filepath.Join(hubEntry, "blobs", testEtagConfig), "config-content")
	mustWrite(t, filepath.Join(hubEntry, "blobs", testEtagModel), "model-weights-0123456789")
	mustWrite(t, filepath.Join(hubEntry, "refs", "main"), testCommit)
	mustSymlink(t, "../../blobs/"+testEtagConfig, filepath.Join(hubEntry, "snapshots", testCommit, "config.json"))
	mustSymlink(t, "../../../blobs/"+testEtagModel, filepath.Join(hubEntry, "snapshots", testCommit, "subdir", "model.safetensors"))

	// Escaping symlink: points OUTSIDE the cache root entirely.
	outsideDir := t.TempDir()
	mustWrite(t, filepath.Join(outsideDir, "secret.txt"), "top-secret")
	mustSymlink(t, filepath.Join(outsideDir, "secret.txt"), filepath.Join(hubEntry, "snapshots", testCommit, "evil.txt"))

	// --- GGUF entry ----------------------------------------------------------
	mustMkdir(t, filepath.Join(cacheRoot, "llamacpp"))
	mustWrite(t, filepath.Join(cacheRoot, "llamacpp", "model.gguf"), "gguf-bytes-content")
	mustWrite(t, filepath.Join(cacheRoot, "llamacpp", "other.gguf"), "unindexed-do-not-serve")

	// --- Ollama entry: manifest + referenced blobs + one unreferenced blob ---
	manifestDir := filepath.Join(cacheRoot, "ollama", "manifests", "registry.ollama.ai", "library", "llama3")
	mustMkdir(t, manifestDir)
	mustMkdir(t, filepath.Join(cacheRoot, "ollama", "blobs"))
	manifest := fmt.Sprintf(`{"config":{"digest":"sha256:%s"},"layers":[{"digest":"sha256:%s"}]}`, testDigestConfig, testDigestLayer)
	mustWrite(t, filepath.Join(manifestDir, "latest"), manifest)
	mustWrite(t, filepath.Join(cacheRoot, "ollama", "blobs", "sha256-"+testDigestConfig), "ollama-config-blob")
	mustWrite(t, filepath.Join(cacheRoot, "ollama", "blobs", "sha256-"+testDigestLayer), "ollama-layer-weights")
	mustWrite(t, filepath.Join(cacheRoot, "ollama", "blobs", "sha256-"+testDigestUnref), "unreferenced-blob")

	// --- Index ---------------------------------------------------------------
	store := cacheindex.Open(filepath.Join(t.TempDir(), "cache-index.json"), nil)
	mustUpsert(t, store, cacheindex.Entry{
		CacheDir: services.HFHubCacheDirName, Family: services.CacheFamilyHFHub,
		Model: "org/repo", Engine: "vllm", Files: []string{"models--org--repo"}, SizeBytes: 40,
	})
	mustUpsert(t, store, cacheindex.Entry{
		CacheDir: services.LlamaCppCacheDirName, Family: services.CacheFamilyGGUFDir,
		Model: "me/mymodel", Engine: "llamacpp", Files: []string{"model.gguf"}, SizeBytes: 18,
	})
	mustUpsert(t, store, cacheindex.Entry{
		CacheDir: services.EngineCacheDirs["ollama"].Dir, Family: services.CacheFamilyNative,
		Model: "llama3", Engine: "ollama", SizeBytes: 100,
	})
	mustUpsert(t, store, cacheindex.Entry{
		CacheDir: "lmstudio", Family: services.CacheFamilyNative,
		Model: "_store", Engine: "lmstudio", SizeBytes: 999,
	})

	provider := func() (*cacheindex.Index, string) { return store.Snapshot(), cacheRoot }
	return provider, cacheRoot
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func mustUpsert(t *testing.T, store *cacheindex.Store, e cacheindex.Entry) {
	t.Helper()
	if err := store.Upsert(e); err != nil {
		t.Fatalf("upsert %s/%s: %v", e.CacheDir, e.Model, err)
	}
}

// authorizedPerms returns permissions with the Files capability enabled and the
// test passcode set (legacy bcrypt path — no vault hooks wired in tests).
func authorizedPerms(t *testing.T) *config.Permissions {
	t.Helper()
	p := config.DefaultPermissions()
	p.Files = true
	if err := p.SetPasscode(testPasscode); err != nil {
		t.Fatalf("SetPasscode: %v", err)
	}
	return p
}

func okResolver() *MockMeshResolver {
	return &MockMeshResolver{Identity: &MeshPeerIdentity{NodeName: "peer", LoginName: "a@b.co", SameOwner: true}}
}

// newCacheGateway builds a gateway with the cache routes registered through the
// SAME registerCacheRoutes path Start uses.
func newCacheGateway(provider CacheIndexProvider, resolver MeshIdentityResolver, perms *config.Permissions) *Server {
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.SetCacheServer(provider)
	if resolver != nil {
		gw.SetMeshResolver(resolver)
	}
	if perms != nil {
		gw.SetPermissions(perms)
	}
	gw.registerCacheRoutes()
	gw.mux.HandleFunc("/", gw.handleRoot)
	return gw
}

func cacheReq(method, target, passcode string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "100.64.0.9:1234"
	if passcode != "" {
		r.Header.Set("X-Citadel-Passcode", passcode)
	}
	return r
}

// allCacheRoutes enumerates one GET request per route family, for the structural
// "authorization applies before every read" matrix.
func allCacheRoutes() []struct{ name, target string } {
	return []struct{ name, target string }{
		{"index", cacheIndexPath},
		{"hf-tree", "/cache/hf/api/models/org/repo"},
		{"hf-file", "/cache/hf/org/repo/resolve/main/config.json"},
		{"gguf", "/cache/gguf/llamacpp/model.gguf"},
		{"ollama-manifest", "/cache/ollama/manifest?model=llama3"},
		{"ollama-blob", "/cache/ollama/blob?model=llama3&digest=sha256:" + testDigestLayer},
	}
}

// TestCacheServe_CategoryAlwaysAllowed pins that /cache/ routes are always-
// allowed at the capability layer, so permissionMiddleware never runs its
// passcode check before the handler's identity-first gate.
func TestCacheServe_CategoryAlwaysAllowed(t *testing.T) {
	for _, p := range []string{cacheIndexPath, "/cache/hf/org/repo/resolve/main/x", "/cache/gguf/llamacpp/m.gguf", "/cache/ollama/manifest"} {
		if got := categoryForPath(p); got != "" {
			t.Errorf("categoryForPath(%q) = %q, want \"\" (sole-gated by the cache handler)", p, got)
		}
	}
}

// TestCacheServe_AuthMatrix proves that EVERY cache route denies under each
// missing-auth condition, and serves under full authorization — the structural
// proof that authorization applies before both index metadata and file reads.
func TestCacheServe_AuthMatrix(t *testing.T) {
	provider, _ := buildCacheFixture(t)

	type gwCase struct {
		name     string
		resolver MeshIdentityResolver
		perms    *config.Permissions
		passcode string
		want     int
	}
	cases := []gwCase{
		{"authorized", okResolver(), authorizedPerms(t), testPasscode, http.StatusOK},
		{"nil-resolver", nil, authorizedPerms(t), testPasscode, http.StatusForbidden},
		{"resolver-error", &MockMeshResolver{Err: fmt.Errorf("not a mesh peer")}, authorizedPerms(t), testPasscode, http.StatusForbidden},
		{"wrong-identity", &MockMeshResolver{Identity: &MeshPeerIdentity{SameOwner: false}}, authorizedPerms(t), testPasscode, http.StatusForbidden},
		{"nil-perms", okResolver(), nil, testPasscode, http.StatusForbidden},
		{"files-disabled", okResolver(), func() *config.Permissions { p := authorizedPerms(t); p.Files = false; return p }(), testPasscode, http.StatusForbidden},
		{"missing-passcode", okResolver(), authorizedPerms(t), "", http.StatusUnauthorized},
		{"wrong-passcode", okResolver(), authorizedPerms(t), "wrong", http.StatusUnauthorized},
	}

	for _, gc := range cases {
		for _, route := range allCacheRoutes() {
			t.Run(gc.name+"/"+route.name, func(t *testing.T) {
				gw := newCacheGateway(provider, gc.resolver, gc.perms)
				rec := httptest.NewRecorder()
				gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, route.target, gc.passcode))
				if rec.Code != gc.want {
					t.Fatalf("route %s: got %d, want %d (body=%s)", route.target, rec.Code, gc.want, rec.Body.String())
				}
			})
		}
	}
}

// TestCacheServe_WriteMethodsRejected asserts a non-read method is 405 even with
// full authorization.
func TestCacheServe_WriteMethodsRejected(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		for _, route := range allCacheRoutes() {
			rec := httptest.NewRecorder()
			gw.mux.ServeHTTP(rec, cacheReq(m, route.target, testPasscode))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got %d, want 405", m, route.target, rec.Code)
			}
		}
	}
}

// TestCacheServe_BearerPasscodeAccepted verifies the HF-client transport
// (Authorization: Bearer <passcode>) authenticates the cache gate.
func TestCacheServe_BearerPasscodeAccepted(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))
	r := httptest.NewRequest(http.MethodGet, cacheIndexPath, nil)
	r.RemoteAddr = "100.64.0.9:1234"
	r.Header.Set("Authorization", "Bearer "+testPasscode)
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer-authed index: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestCacheServe_FullChainThroughBuildHandler proves the whole middleware chain
// (logging + permission + exposure + mux) passes a cache request through to the
// handler, and that a disabled Files capability is denied by the handler (not
// silently allowed by permissionMiddleware's category="").
func TestCacheServe_FullChainThroughBuildHandler(t *testing.T) {
	provider, _ := buildCacheFixture(t)

	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))
	rec := httptest.NewRecorder()
	gw.BuildHandler().ServeHTTP(rec, cacheReq(http.MethodGet, cacheIndexPath, testPasscode))
	if rec.Code != http.StatusOK {
		t.Fatalf("full-chain authorized index: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	disabled := authorizedPerms(t)
	disabled.Files = false
	gw2 := newCacheGateway(provider, okResolver(), disabled)
	rec2 := httptest.NewRecorder()
	gw2.BuildHandler().ServeHTTP(rec2, cacheReq(http.MethodGet, cacheIndexPath, testPasscode))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("full-chain files-disabled index: got %d, want 403 (body=%s)", rec2.Code, rec2.Body.String())
	}
}

// TestCacheServe_Index lists indexed entries with availability, marking the
// unsupported native aggregate row unavailable rather than omitting it.
func TestCacheServe_Index(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, cacheIndexPath, testPasscode))
	if rec.Code != http.StatusOK {
		t.Fatalf("index: got %d, want 200", rec.Code)
	}
	var resp struct {
		Entries []cacheIndexEntryView `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	byModel := map[string]cacheIndexEntryView{}
	for _, e := range resp.Entries {
		byModel[e.CacheDir+"/"+e.Model] = e
	}
	if len(resp.Entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(resp.Entries), resp.Entries)
	}
	for _, want := range []string{"huggingface/org/repo", "llamacpp/me/mymodel", "ollama/llama3"} {
		if e, ok := byModel[want]; !ok || !e.Available {
			t.Errorf("entry %q: available=%v ok=%v, want available", want, e.Available, ok)
		}
	}
	agg := byModel["lmstudio/_store"]
	if agg.Available || agg.Reason != "unsupported" {
		t.Errorf("aggregate row: available=%v reason=%q, want unavailable/unsupported", agg.Available, agg.Reason)
	}
}

// TestCacheServe_HFTree returns the revision sha and file tree, excluding the
// escaping symlink.
func TestCacheServe_HFTree(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/hf/api/models/org/repo", testPasscode))
	if rec.Code != http.StatusOK {
		t.Fatalf("hf tree: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sha      string      `json:"sha"`
		Siblings []hfSibling `json:"siblings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Sha != testCommit {
		t.Errorf("sha = %q, want %q", resp.Sha, testCommit)
	}
	names := map[string]int64{}
	for _, s := range resp.Siblings {
		names[s.RFilename] = s.Size
	}
	if _, ok := names["config.json"]; !ok {
		t.Errorf("tree missing config.json: %+v", resp.Siblings)
	}
	if _, ok := names["subdir/model.safetensors"]; !ok {
		t.Errorf("tree missing subdir/model.safetensors: %+v", resp.Siblings)
	}
	if _, ok := names["evil.txt"]; ok {
		t.Errorf("escaping symlink evil.txt must NOT be listed: %+v", resp.Siblings)
	}
	if got := names["config.json"]; got != int64(len("config-content")) {
		t.Errorf("config.json size = %d, want %d", got, len("config-content"))
	}
}

// TestCacheServe_HFFile serves the resolved blob with HF headers and Range.
func TestCacheServe_HFFile(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))

	// Full GET.
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/hf/org/repo/resolve/main/config.json", testPasscode))
	if rec.Code != http.StatusOK {
		t.Fatalf("hf file: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "config-content" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "config-content")
	}
	if got := rec.Header().Get("X-Repo-Commit"); got != testCommit {
		t.Errorf("X-Repo-Commit = %q, want %q", got, testCommit)
	}
	if got := rec.Header().Get("ETag"); got != `"`+testEtagConfig+`"` {
		t.Errorf("ETag = %q, want %q", got, `"`+testEtagConfig+`"`)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len("config-content")) {
		t.Errorf("Content-Length = %q, want %d", got, len("config-content"))
	}

	// Range request.
	rr := cacheReq(http.MethodGet, "/cache/hf/org/repo/resolve/main/config.json", testPasscode)
	rr.Header.Set("Range", "bytes=0-5")
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, rr)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range GET: got %d, want 206", rec.Code)
	}
	if rec.Body.String() != "config" {
		t.Errorf("range body = %q, want %q", rec.Body.String(), "config")
	}

	// HEAD carries headers, no body.
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodHead, "/cache/hf/org/repo/resolve/main/config.json", testPasscode))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD: got %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len("config-content")) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, len("config-content"))
	}
}

// TestCacheServe_HFRejections covers unknown model, unknown revision, traversal
// revision, and the escaping snapshot symlink.
func TestCacheServe_HFRejections(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))
	for _, tc := range []struct {
		name, target string
	}{
		{"unknown-model", "/cache/hf/org/other/resolve/main/config.json"},
		{"unknown-revision", "/cache/hf/org/repo/resolve/nope/config.json"},
		{"traversal-revision", "/cache/hf/org/repo/resolve/../config.json"},
		{"escaping-symlink", "/cache/hf/org/repo/resolve/main/evil.txt"},
		{"unindexed-file", "/cache/hf/org/repo/resolve/main/nonexistent.bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, tc.target, testPasscode))
			if rec.Code == http.StatusOK {
				t.Fatalf("%s: got 200, want a rejection (body=%s)", tc.target, rec.Body.String())
			}
		})
	}
}

// TestCacheServe_GGUF serves an exact indexed file and rejects an unindexed one.
func TestCacheServe_GGUF(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))

	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/gguf/llamacpp/model.gguf", testPasscode))
	if rec.Code != http.StatusOK || rec.Body.String() != "gguf-bytes-content" {
		t.Fatalf("gguf indexed file: got %d body=%q, want 200 gguf-bytes-content", rec.Code, rec.Body.String())
	}

	// other.gguf exists on disk but is not an indexed file -> 404.
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/gguf/llamacpp/other.gguf", testPasscode))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("gguf unindexed file: got %d, want 404", rec.Code)
	}

	// unknown cache dir -> 404.
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/gguf/nope/model.gguf", testPasscode))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("gguf unknown dir: got %d, want 404", rec.Code)
	}
}

// TestCacheServe_Ollama serves the indexed manifest and referenced blobs, and
// rejects an unreferenced blob and an unindexed model.
func TestCacheServe_Ollama(t *testing.T) {
	provider, _ := buildCacheFixture(t)
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))

	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/ollama/manifest?model=llama3", testPasscode))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testDigestLayer) {
		t.Fatalf("ollama manifest: got %d body=%q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/ollama/blob?model=llama3&digest=sha256:"+testDigestLayer, testPasscode))
	if rec.Code != http.StatusOK || rec.Body.String() != "ollama-layer-weights" {
		t.Fatalf("ollama referenced blob: got %d body=%q", rec.Code, rec.Body.String())
	}

	// Unreferenced blob (exists on disk, not in the manifest) -> 404.
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/ollama/blob?model=llama3&digest=sha256:"+testDigestUnref, testPasscode))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ollama unreferenced blob: got %d, want 404", rec.Code)
	}

	// Unindexed model -> 404.
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/ollama/manifest?model=not-indexed", testPasscode))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ollama unindexed model: got %d, want 404", rec.Code)
	}

	// Malformed digest -> 404.
	rec = httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, "/cache/ollama/blob?model=llama3&digest=notadigest", testPasscode))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ollama malformed digest: got %d, want 404", rec.Code)
	}
}

// TestCacheServe_ProviderUnavailable degrades to 404 when the provider returns
// no index (cache serving effectively disabled), still after passing the gate.
func TestCacheServe_ProviderUnavailable(t *testing.T) {
	provider := func() (*cacheindex.Index, string) { return nil, "" }
	gw := newCacheGateway(provider, okResolver(), authorizedPerms(t))
	rec := httptest.NewRecorder()
	gw.mux.ServeHTTP(rec, cacheReq(http.MethodGet, cacheIndexPath, testPasscode))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil provider index: got %d, want 404", rec.Code)
	}
}

// TestCacheServe_UnitHelpers pins the small validators the path-derived inputs
// rely on.
func TestCacheServe_UnitHelpers(t *testing.T) {
	if isSingleSegment("..") || isSingleSegment(".") || isSingleSegment("") || isSingleSegment("a/b") || isSingleSegment("a\\b") {
		t.Error("isSingleSegment accepted a traversing/empty segment")
	}
	if !isSingleSegment("main") || !isSingleSegment("registry.ollama.ai") {
		t.Error("isSingleSegment rejected a valid segment")
	}
	if !isHexCommit(testCommit) || isHexCommit("nope") || isHexCommit("abc") {
		t.Error("isHexCommit wrong")
	}
	if !isSha256Digest("sha256:"+testDigestLayer) || isSha256Digest("sha256:zz") || isSha256Digest(testDigestLayer) {
		t.Error("isSha256Digest wrong")
	}
	host, ns, name, tag, ok := parseOllamaRef("llama3")
	if !ok || host != "registry.ollama.ai" || ns != "library" || name != "llama3" || tag != "latest" {
		t.Errorf("parseOllamaRef(llama3) = %q %q %q %q ok=%v", host, ns, name, tag, ok)
	}
	host, ns, name, tag, ok = parseOllamaRef("myorg/mymodel:q4")
	if !ok || host != "registry.ollama.ai" || ns != "myorg" || name != "mymodel" || tag != "q4" {
		t.Errorf("parseOllamaRef(myorg/mymodel:q4) = %q %q %q %q ok=%v", host, ns, name, tag, ok)
	}
	if _, _, _, _, ok := parseOllamaRef("a/../b"); ok {
		t.Error("parseOllamaRef accepted a traversing segment")
	}
}
