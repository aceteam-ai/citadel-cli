package gateway

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// buildExposedDirGateway wires a gateway serving root as a directory-source
// exposure under name, with MAXIMALLY RESTRICTIVE capability permissions
// (mirrors buildExposedGateway in exposure_test.go) so these tests prove
// exposureMiddleware + the confinement handler are the SOLE gate for a
// directory share too — not just the port-proxy source.
func buildExposedDirGateway(t *testing.T, name, root string, policy *ExposePolicy, resolver MeshIdentityResolver) http.Handler {
	t.Helper()
	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.SetPermissions(&config.Permissions{})
	gw.SetMeshResolver(resolver)
	if err := gw.ExposeDir(name, root, policy); err != nil {
		t.Fatalf("ExposeDir: %v", err)
	}
	// Start's loop is not run in tests; register directly (mirrors
	// buildExposedGateway's manual registerProxy loop).
	for prefix, dr := range gw.dirExposures {
		gw.registerDirHandler(prefix, dr)
	}
	return gw.BuildHandler()
}

// orgResolver is a same-owner MockMeshResolver, used throughout so these
// tests exercise the confinement handler rather than re-testing the
// visibility ladder (already covered by exposure_test.go).
func orgResolver() MeshIdentityResolver {
	return &MockMeshResolver{Identity: &MeshPeerIdentity{LoginName: "a@b.co", SameOwner: true}}
}

var orgPolicy = &ExposePolicy{Visibility: VisibilityOrg}

// TestExposeDir_ServesNormalFile is the control case: a normal file inside
// the exposed directory must serve, with its content intact.
func TestExposeDir_ServesNormalFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := buildExposedDirGateway(t, "docs", root, orgPolicy, orgResolver())

	w := doGet(h, "/expose/docs/hello.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("normal file: got %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello world" {
		t.Errorf("normal file body = %q, want %q", w.Body.String(), "hello world")
	}
}

// TestExposeDir_SymlinkEscapeRejected is THE acceptance-bar test named in the
// issue: a symlink planted inside the exposed directory that points OUTSIDE
// the confined root must never let a request read what it points to, even
// though the lexical request path never contains "..". This is the exact
// http.Dir landmine ExposeDir exists to close.
func TestExposeDir_SymlinkEscapeRejected(t *testing.T) {
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP-SECRET-DO-NOT-LEAK"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	escapeLink := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escapeLink); err != nil {
		t.Fatalf("os.Symlink: %v (symlinks may be unavailable in this environment)", err)
	}

	h := buildExposedDirGateway(t, "docs", root, orgPolicy, orgResolver())

	// Sanity: the file WITHIN root still serves (proves the handler is wired,
	// not just failing everything).
	if w := doGet(h, "/expose/docs/hello.txt"); w.Code != http.StatusOK {
		t.Fatalf("sanity check (in-root file): got %d, want 200", w.Code)
	}

	// The escape: requesting through the symlink must NOT return the secret.
	w := doGet(h, "/expose/docs/escape/secret.txt")
	if w.Code == http.StatusOK {
		t.Fatalf("symlink escape was SERVED (got 200): body=%q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "TOP-SECRET") {
		t.Fatalf("symlink escape leaked secret content despite non-200 status %d: body=%q", w.Code, w.Body.String())
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("symlink escape: got %d, want 404 (fail closed as not-found)", w.Code)
	}
}

// TestExposeDir_PathTraversalRejected covers plain, encoded, and
// absolute-looking traversal attempts. Go's net/http mux and filepath.Join's
// own semantics neutralize most of these lexically before the confinement
// check even runs, but the assertion is on the observable behavior (never
// 200 with content from outside root), not on which layer caught it.
func TestExposeDir_PathTraversalRejected(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := buildExposedDirGateway(t, "docs", root, orgPolicy, orgResolver())

	cases := []struct {
		name   string
		target string
	}{
		{"plain dot-dot", "/expose/docs/../../../../../../../../etc/passwd"},
		{"encoded dot-dot", "/expose/docs/%2e%2e/%2e%2e/%2e%2e/etc/passwd"},
		{"dot-dot to sibling tempdir secret", "/expose/docs/" + strings.Repeat("../", 20) + strings.TrimPrefix(filepath.Join(outside, "secret.txt"), "/")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.target, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code == http.StatusOK {
				t.Fatalf("%s: served 200 (body=%q), want rejected", c.name, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "TOP-SECRET") || strings.Contains(w.Body.String(), "root:") {
				t.Fatalf("%s: leaked content outside the confined root (status=%d body=%q)", c.name, w.Code, w.Body.String())
			}
		})
	}
}

// TestExposeDir_AbsolutePathSegmentDoesNotEscape asserts the property
// directly against the confinement primitive (not through the mux, which
// would 301-redirect an unclean path before ever reaching the handler): an
// absolute-looking relative segment must still resolve INSIDE root, mirroring
// Go's filepath.Join semantics (it never lets a later absolute-looking
// component replace the first).
func TestExposeDir_AbsolutePathSegmentDoesNotEscape(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := resolveConfinedRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = resolveConfinedTarget(resolvedRoot, "/etc/passwd")
	if err == nil {
		t.Fatal("resolveConfinedTarget accepted an absolute-looking segment that does not exist under root; want an error")
	}
}

// TestExposeDir_ResolveConfinedTarget_UnitLevel exercises the confinement
// primitive directly with a battery of malicious relative paths, independent
// of net/http's own path-cleaning behavior — a future caller of this function
// (or a change to how the mux dispatches) must not silently lose this
// protection.
func TestExposeDir_ResolveConfinedTarget_UnitLevel(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolvedRoot, err := resolveConfinedRoot(root)
	if err != nil {
		t.Fatal(err)
	}

	// Control: a real file inside root resolves fine.
	if _, info, err := resolveConfinedTarget(resolvedRoot, "/ok.txt"); err != nil || info.IsDir() {
		t.Fatalf("control case failed: info=%v err=%v", info, err)
	}

	escapeAttempts := []string{
		"/../" + filepath.Base(outside) + "/secret.txt",
		"/./../" + filepath.Base(outside) + "/secret.txt",
		"/" + strings.Repeat("../", 15) + strings.TrimPrefix(filepath.Join(outside, "secret.txt"), "/"),
	}
	for _, rel := range escapeAttempts {
		if resolved, _, err := resolveConfinedTarget(resolvedRoot, rel); err == nil {
			t.Errorf("resolveConfinedTarget(%q) resolved to %q, want rejected", rel, resolved)
		}
	}
}

// TestExposeDir_AutoIndex covers the auto-index requirement (#943): a
// directory with no index.html renders a listing containing the directory's
// entries.
func TestExposeDir_AutoIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "report.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	h := buildExposedDirGateway(t, "docs", root, orgPolicy, orgResolver())

	w := doGet(h, "/expose/docs/")
	if w.Code != http.StatusOK {
		t.Fatalf("auto-index: got %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "report.txt") {
		t.Errorf("auto-index missing file entry: %s", body)
	}
	if !strings.Contains(body, "subdir/") {
		t.Errorf("auto-index missing directory entry: %s", body)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("auto-index Content-Type = %q, want text/html", ct)
	}
}

// TestExposeDir_BareNameRedirectsToTrailingSlash asserts requesting the exact
// exposure name (no trailing slash) redirects to the trailing-slash form, so
// relative links in an auto-index or index.html resolve correctly.
func TestExposeDir_BareNameRedirectsToTrailingSlash(t *testing.T) {
	root := t.TempDir()
	h := buildExposedDirGateway(t, "docs", root, orgPolicy, orgResolver())

	w := doGet(h, "/expose/docs")
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("bare name: got %d, want 301", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/expose/docs/" {
		t.Errorf("redirect Location = %q, want /expose/docs/", loc)
	}
}

// TestExposeDir_ServesIndexHTMLWhenPresent asserts index.html is preferred
// over the auto-index listing when the directory has one.
func TestExposeDir_ServesIndexHTMLWhenPresent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>custom index</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := buildExposedDirGateway(t, "docs", root, orgPolicy, orgResolver())

	w := doGet(h, "/expose/docs/")
	if w.Code != http.StatusOK {
		t.Fatalf("index.html: got %d, want 200", w.Code)
	}
	if w.Body.String() != "<h1>custom index</h1>" {
		t.Errorf("index.html body = %q, want the custom index content (not the auto-index)", w.Body.String())
	}
}

// TestExposeDir_UnregisteredNameIs404 proves exposureMiddleware is still the
// SOLE gate for a directory-source exposure — an unregistered/never-exposed
// name must 404, same as the proxy source (#598's core invariant, extended
// here to the new source type).
func TestExposeDir_UnregisteredNameIs404(t *testing.T) {
	root := t.TempDir()
	h := buildExposedDirGateway(t, "docs", root, orgPolicy, orgResolver())

	w := doGet(h, "/expose/never-exposed/hello.txt")
	if w.Code != http.StatusNotFound {
		t.Errorf("unregistered name: got %d, want 404", w.Code)
	}
}

// TestExposeDir_UnexposeStopsServing proves Unexpose tears down a directory
// share's live root, not just its policy -- defense in depth beyond the
// policy-only 404 exposureMiddleware already provides.
func TestExposeDir_UnexposeStopsServing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	gw := NewServer(Config{Port: 0, NodeName: "test-node"})
	gw.SetPermissions(&config.Permissions{})
	gw.SetMeshResolver(orgResolver())
	if err := gw.ExposeDir("docs", root, orgPolicy); err != nil {
		t.Fatal(err)
	}
	for prefix, dr := range gw.dirExposures {
		gw.registerDirHandler(prefix, dr)
	}
	h := gw.BuildHandler()

	if w := doGet(h, "/expose/docs/hello.txt"); w.Code != http.StatusOK {
		t.Fatalf("before unexpose: got %d, want 200", w.Code)
	}
	if !gw.Unexpose("docs") {
		t.Fatal("Unexpose reported nothing was exposed")
	}
	if w := doGet(h, "/expose/docs/hello.txt"); w.Code == http.StatusOK {
		t.Errorf("after unexpose: still served 200, want denied")
	}
	if got := gw.dirExposures[ExposeRoutePath("docs")].get(); got != "" {
		t.Errorf("dirRoot after Unexpose = %q, want empty", got)
	}
}

// TestExposeDir_CrossTypeNameCollisionRejected proves a name cannot be
// re-registered under the OTHER source type: since http.ServeMux has no
// deregister, doing so would double-register the same mux pattern and panic
// the whole gateway. Both directions are tested.
func TestExposeDir_CrossTypeNameCollisionRejected(t *testing.T) {
	t.Run("dir then port", func(t *testing.T) {
		gw := NewServer(Config{Port: 0})
		root := t.TempDir()
		if err := gw.ExposeDir("svc", root, orgPolicy); err != nil {
			t.Fatal(err)
		}
		if err := gw.Expose("svc", "127.0.0.1:9999", orgPolicy); err == nil {
			t.Fatal("Expose over an existing directory-source name should have been rejected")
		}
	})
	t.Run("port then dir", func(t *testing.T) {
		gw := NewServer(Config{Port: 0})
		root := t.TempDir()
		if err := gw.Expose("svc", "127.0.0.1:9999", orgPolicy); err != nil {
			t.Fatal(err)
		}
		if err := gw.ExposeDir("svc", root, orgPolicy); err == nil {
			t.Fatal("ExposeDir over an existing port-source name should have been rejected")
		}
	})
}

// TestExposeDir_RejectsNonDirectory / RejectsMissing / RejectsRelative pin
// resolveConfinedRoot's own input validation.
func TestExposeDir_RejectsNonDirectoryOrMissingOrRelative(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []string{
		file,                                 // a file, not a directory
		filepath.Join(dir, "does-not-exist"), // missing
		"relative/path",                      // not absolute
		"",                                   // empty
	}
	for _, c := range cases {
		gw := NewServer(Config{Port: 0})
		if err := gw.ExposeDir("svc", c, orgPolicy); err == nil {
			t.Errorf("ExposeDir(%q) should have been rejected", c)
		}
	}
}
