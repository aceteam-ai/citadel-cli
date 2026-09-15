// internal/gateway/cache_serve.go
//
// Authenticated, read-only model-cache serving for authorized peers
// (citadel-cli#1013). This adds a bounded read surface over the node's durable
// cache index (internal/cacheindex) so an authorized same-owner mesh peer can
// inspect and retrieve ALREADY-cached model artifacts without a fresh upstream
// download. It is SERVING ONLY: peer selection, pulling, retention, and
// eviction are separate work and deliberately not touched here.
//
// # The gate (recorded per the issue's requirement)
//
// A cache request must satisfy, in this order, ALL of:
//
//  1. Verified same-owner MESH PEER identity — the SAME primitive `org`
//     exposures use (meshResolver -> network.WhoIsPeer, MeshPeerIdentity.
//     SameOwner). This is the "authorized peer / authenticated node access"
//     boundary. It is checked FIRST so an unauthenticated caller never reaches
//     the bcrypt/vault passcode check below (which is rate-limited and
//     lockout-gated under a master PIN). It is also what makes these routes
//     safe on EVERY listener they ride: a plain LAN RemoteAddr cannot resolve
//     to a verified same-owner peer, so the route cannot become an
//     unauthenticated LAN route even though it shares the mux with the LAN
//     listener.
//  2. The `files` sensitive CAPABILITY, enabled by the node operator. Serving
//     cached model files is a file-read surface, so it reuses the existing
//     `files` capability rather than minting a new one. Unlike
//     permissionMiddleware (which allows-all when permissions are nil, for
//     backwards compatibility), this gate fails CLOSED on nil/disabled — the
//     issue requires denying when permissions are missing.
//  3. The per-node PASSCODE — because `files` is a sensitive category
//     (config.IsSensitiveCategory), an enabled capability still requires the
//     passcode, and fails closed when none is set. The supported local HF
//     client presents it as `Authorization: Bearer <passcode>` (HF_TOKEN); the
//     header/query forms are accepted too.
//
// The plain-reading alternative — mesh identity + capability only, no passcode
// — is a one-line relaxation (drop step 3): mesh membership is itself strong
// cryptographic node auth, and "the existing verified node-auth boundary" reads
// most naturally as that. The strict union is shipped because the issue asks
// for a "sensitive capability" gate and lists both "missing/invalid auth" and
// "wrong identity" as distinct denial cases.
//
// Listener binding: registered on the shared gateway mux (Start), so the routes
// ride the LAN listener AND the tsnet VPN listener — the same binding as the
// chat routes. Safety on the LAN listener comes from gate step 1, not from
// binding to a single listener.
//
// # Confinement (reused, not reinvented)
//
// Every read goes through this package's existing resolved-root confinement
// (resolveConfinedRoot / resolveConfinedTarget / withinDir, expose_dir.go),
// which EvalSymlinks each request target and rejects anything escaping the
// resolved root — INCLUDING the HF snapshot->blob symlink. The confinement root
// is scoped PER INDEXED ENTRY (the entry's own models--org--repo directory for
// HF, the cache dir for GGUF, the ollama store for ollama), never the shared
// cache root, so a symlink in one repo's snapshots that points into a sibling
// repo's blobs is rejected as "directory outside an indexed entry".
package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aceteam-ai/citadel-cli/internal/cacheindex"
	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/services"
)

// Cache-serving route namespace. Kept under a single /cache/ prefix so the
// capability layer can always-allow the whole namespace (categoryForPath) and
// hand sole gating to authorizeCacheRequest, mirroring /expose/. The HF client
// honors HF_ENDPOINT, so pointing it at https://<node>:<port>/cache/hf yields
// the /cache/hf/<repo>/resolve/... and /cache/hf/api/models/... paths below.
const (
	CacheRoutePrefix  = "/cache/"
	cacheIndexPath    = "/cache/index"
	cacheHFPrefix     = "/cache/hf/"
	cacheGGUFPrefix   = "/cache/gguf/"
	cacheOllamaPrefix = "/cache/ollama/"
)

// cacheCapability is the existing sensitive capability cache serving is gated
// under. Reusing `files` (a file-read surface) avoids introducing a new
// capability/config key for this slice.
const cacheCapability = "files"

// CacheIndexProvider returns a read-only snapshot of the node's cache index and
// the resolved on-disk cache root (~/citadel-cache, cacheindex.DefaultCacheRoot).
// Injected (rather than importing a live *cacheindex.Store) so the gateway is
// decoupled and unit-testable with a synthetic index over a t.TempDir() tree,
// mirroring the ChatModelLister / MeshIdentityResolver injection pattern. A nil
// return (or empty root) means "cache serving unavailable".
type CacheIndexProvider func() (*cacheindex.Index, string)

// SetCacheServer enables the authenticated read-only cache-serving routes
// (/cache/...). When set, Start registers /cache/index and the per-family read
// routes. Passing nil leaves them unregistered (the default). Must be called
// before Start.
func (s *Server) SetCacheServer(provider CacheIndexProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheProvider = provider
}

// registerCacheRoutes wires the cache-serving handlers onto the mux. Called from
// Start (and directly from tests) so tests exercise the SAME registration path
// as production.
func (s *Server) registerCacheRoutes() {
	s.mux.HandleFunc(cacheIndexPath, s.handleCacheIndex)
	s.mux.HandleFunc(cacheHFPrefix, s.handleCacheHF)
	s.mux.HandleFunc(cacheGGUFPrefix, s.handleCacheGGUF)
	s.mux.HandleFunc(cacheOllamaPrefix, s.handleCacheOllama)
}

// cachePasscodeFromRequest extracts the per-node passcode a cache caller
// presents. In addition to the header/query forms passcodeFromRequest reads, it
// accepts `Authorization: Bearer <passcode>` — the transport the supported
// local HF client uses for HF_TOKEN — so the cache routes are usable by that
// client without a second credential. Scoped to the cache gate only; no other
// route consults the Authorization header.
func cachePasscodeFromRequest(r *http.Request) string {
	if p := passcodeFromRequest(r); p != "" {
		return p
	}
	const bearer = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearer) {
		return strings.TrimSpace(strings.TrimPrefix(h, bearer))
	}
	return ""
}

// authorizeCacheRequest is the SOLE gate for cache routes. See the package doc
// for the ordered contract. Returns false (and has already written a JSON error)
// when the request must be denied.
func (s *Server) authorizeCacheRequest(w http.ResponseWriter, r *http.Request) bool {
	s.mu.RLock()
	resolver := s.meshResolver
	perms := s.permissions
	s.mu.RUnlock()

	// 1. Verified same-owner mesh peer (identity FIRST — never reach the passcode
	//    check without a verified peer).
	id, ok := resolvePeer(resolver, r)
	if !ok || !id.SameOwner {
		exposeDeny(w, http.StatusForbidden, "authorized same-owner mesh peer required")
		return false
	}
	// 2. Capability, fail CLOSED on nil/disabled (deliberately unlike
	//    permissionMiddleware's allow-all-on-nil).
	if perms == nil || !perms.Files {
		exposeDeny(w, http.StatusForbidden, "cache serving capability disabled by node operator")
		return false
	}
	// 3. Passcode (sensitive category), fail closed when unset/wrong.
	if config.IsSensitiveCategory(cacheCapability) {
		if !perms.VerifyPasscode(cachePasscodeFromRequest(r)) {
			exposeDeny(w, http.StatusUnauthorized, "node passcode required")
			return false
		}
	}
	return true
}

// beginCacheRequest runs the gate, the read-method check, and resolves the
// index snapshot + cache root — the common preamble for every cache handler, so
// authorization is structurally applied before any index metadata or file read.
func (s *Server) beginCacheRequest(w http.ResponseWriter, r *http.Request) (*cacheindex.Index, string, bool) {
	if !s.authorizeCacheRequest(w, r) {
		return nil, "", false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		exposeDeny(w, http.StatusMethodNotAllowed, "method not allowed")
		return nil, "", false
	}
	s.mu.RLock()
	provider := s.cacheProvider
	s.mu.RUnlock()
	if provider == nil {
		exposeDeny(w, http.StatusNotFound, "cache serving not enabled")
		return nil, "", false
	}
	index, cacheRoot := provider()
	if index == nil || cacheRoot == "" {
		exposeDeny(w, http.StatusNotFound, "cache serving not enabled")
		return nil, "", false
	}
	return index, cacheRoot, true
}

// --- Index listing --------------------------------------------------------

// cacheIndexEntryView is one row of the read-only index listing. Aggregate /
// unsupported entries are INCLUDED with available=false + a reason, never
// omitted and never expanded into an arbitrary directory listing.
type cacheIndexEntryView struct {
	CacheDir  string `json:"cache_dir"`
	Family    string `json:"family"`
	Model     string `json:"model"`
	SizeBytes int64  `json:"size_bytes"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Server) handleCacheIndex(w http.ResponseWriter, r *http.Request) {
	index, cacheRoot, ok := s.beginCacheRequest(w, r)
	if !ok {
		return
	}
	entries := index.All()
	views := make([]cacheIndexEntryView, 0, len(entries))
	for _, e := range entries {
		available, reason := cacheEntryAvailability(index, e, cacheRoot)
		views = append(views, cacheIndexEntryView{
			CacheDir:  e.CacheDir,
			Family:    string(e.Family),
			Model:     e.Model,
			SizeBytes: e.SizeBytes,
			Available: available,
			Reason:    reason,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].CacheDir != views[j].CacheDir {
			return views[i].CacheDir < views[j].CacheDir
		}
		return views[i].Model < views[j].Model
	})
	writeCacheJSON(w, r, map[string]any{"entries": views})
}

// cacheEntryAvailability decides whether an indexed entry can actually be served
// from local state, and if not, why. Supported families: hf-hub, gguf-dir, and
// native ONLY for ollama's per-model entries. A native aggregate row
// (lmstudio/tei "_store") or any unknown family is unsupported.
func cacheEntryAvailability(index *cacheindex.Index, e cacheindex.Entry, cacheRoot string) (bool, string) {
	switch e.Family {
	case services.CacheFamilyHFHub:
		if len(e.Files) == 0 {
			return false, "missing"
		}
		if _, err := resolveConfinedRoot(filepath.Join(cacheRoot, e.CacheDir, "hub", e.Files[0])); err != nil {
			return false, "missing"
		}
		return true, ""
	case services.CacheFamilyGGUFDir:
		if _, ok := index.Verify(e, cacheRoot); ok {
			return true, ""
		}
		return false, "missing"
	case services.CacheFamilyNative:
		if e.CacheDir != ollamaCacheDir() {
			return false, "unsupported"
		}
		if _, ok := resolveOllamaManifest(cacheRoot, e.Model); !ok {
			return false, "missing"
		}
		return true, ""
	default:
		return false, "unsupported"
	}
}

// --- HuggingFace hub read surface -----------------------------------------

// hfSibling is one file in an HF repo revision tree (the huggingface_hub
// model-info shape a listing client reads).
type hfSibling struct {
	RFilename string `json:"rfilename"`
	Size      int64  `json:"size"`
}

// handleCacheHF serves the HF-client-compatible surface for indexed hf-hub
// entries:
//
//	GET /cache/hf/api/models/<repo>[/revision/<rev>]  -> {sha, siblings[]}
//	GET|HEAD /cache/hf/<repo>/resolve/<rev>/<file>    -> file bytes (+ Range)
func (s *Server) handleCacheHF(w http.ResponseWriter, r *http.Request) {
	index, cacheRoot, ok := s.beginCacheRequest(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, cacheHFPrefix)
	if rest == "" {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}

	if after, isAPI := strings.CutPrefix(rest, "api/models/"); isAPI {
		repo := after
		rev := "main"
		if i := strings.Index(after, "/revision/"); i >= 0 {
			repo = after[:i]
			rev = after[i+len("/revision/"):]
		}
		s.serveHFTree(w, r, index, cacheRoot, repo, rev)
		return
	}

	i := strings.Index(rest, "/resolve/")
	if i < 0 {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	repo := rest[:i]
	after := rest[i+len("/resolve/"):]
	j := strings.IndexByte(after, '/')
	if j < 0 {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	rev := after[:j]
	file := after[j+1:]
	s.serveHFFile(w, r, index, cacheRoot, repo, rev, file)
}

// hfEntryRoot resolves the confined per-entry root (the models--org--repo
// directory) for an indexed hf-hub repo, or ok=false when the repo is not
// indexed or its directory is not on disk.
func hfEntryRoot(index *cacheindex.Index, cacheRoot, repo string) (entryRoot string, ok bool) {
	entry, found := lookupHFEntry(index, repo)
	if !found || len(entry.Files) == 0 {
		return "", false
	}
	root, err := resolveConfinedRoot(filepath.Join(cacheRoot, entry.CacheDir, "hub", entry.Files[0]))
	if err != nil {
		return "", false
	}
	return root, true
}

func (s *Server) serveHFTree(w http.ResponseWriter, r *http.Request, index *cacheindex.Index, cacheRoot, repo, rev string) {
	entryRoot, ok := hfEntryRoot(index, cacheRoot, repo)
	if !ok {
		exposeDeny(w, http.StatusNotFound, "model not found")
		return
	}
	commit, ok := resolveHFCommit(entryRoot, rev)
	if !ok {
		exposeDeny(w, http.StatusNotFound, "unknown revision")
		return
	}
	siblings := listHFTree(entryRoot, commit)
	writeCacheJSON(w, r, map[string]any{
		"id":       repo,
		"modelId":  repo,
		"sha":      commit,
		"siblings": siblings,
	})
}

func (s *Server) serveHFFile(w http.ResponseWriter, r *http.Request, index *cacheindex.Index, cacheRoot, repo, rev, file string) {
	entryRoot, ok := hfEntryRoot(index, cacheRoot, repo)
	if !ok {
		exposeDeny(w, http.StatusNotFound, "model not found")
		return
	}
	commit, ok := resolveHFCommit(entryRoot, rev)
	if !ok {
		exposeDeny(w, http.StatusNotFound, "unknown revision")
		return
	}
	if strings.TrimSpace(file) == "" {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	snapshotPrefix := "/snapshots/" + commit + "/"
	cleanRel := path.Clean(snapshotPrefix + file)
	// Reject a file that climbs above its snapshot directory lexically; the
	// resolved-root confinement below is the real check, but reject early.
	if !strings.HasPrefix(cleanRel, snapshotPrefix) {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	resolved, info, err := resolveConfinedTarget(entryRoot, cleanRel)
	if err != nil || info.IsDir() {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	// HF client metadata. The blob filename in the hub cache IS the file's etag
	// (git sha for small files, sha256 for LFS). http.ServeContent honors a
	// pre-set ETag for If-None-Match/If-Range and handles Range/206/HEAD.
	w.Header().Set("X-Repo-Commit", commit)
	w.Header().Set("ETag", `"`+filepath.Base(resolved)+`"`)
	serveConfinedFile(w, r, resolved, info)
}

// lookupHFEntry finds the indexed hf-hub entry for repo (case-insensitive model
// id match). All hf-hub engines share the "huggingface" cache dir, but this
// scans by family+model so it never assumes a specific cache dir.
func lookupHFEntry(index *cacheindex.Index, repo string) (cacheindex.Entry, bool) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return cacheindex.Entry{}, false
	}
	for _, e := range index.All() {
		if e.Family == services.CacheFamilyHFHub && strings.EqualFold(e.Model, repo) {
			return e, true
		}
	}
	return cacheindex.Entry{}, false
}

// resolveHFCommit resolves an HF revision (a branch/tag name, or an explicit
// commit hash) to the on-disk snapshot commit, entirely from local state (no
// upstream request). rev must be a single path segment. A branch/tag is read
// from refs/<rev>; the resulting commit must be hex AND have a snapshots/<commit>
// directory. Every path-derived value (rev, and the CONTENTS of the refs file)
// is validated before being joined into a path — a tampered refs file is a
// traversal vector the confinement below would also catch, but is rejected
// earlier.
func resolveHFCommit(entryRoot, rev string) (string, bool) {
	if !isSingleSegment(rev) {
		return "", false
	}
	// rev is already a commit hash with a snapshot on disk.
	if isHexCommit(rev) {
		if _, info, err := resolveConfinedTarget(entryRoot, "/snapshots/"+rev); err == nil && info.IsDir() {
			return rev, true
		}
	}
	// Otherwise resolve the ref file to a commit.
	resolved, info, err := resolveConfinedTarget(entryRoot, "/refs/"+rev)
	if err != nil || info.IsDir() {
		return "", false
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", false
	}
	commit := strings.TrimSpace(string(data))
	if !isHexCommit(commit) {
		return "", false
	}
	if _, info, err := resolveConfinedTarget(entryRoot, "/snapshots/"+commit); err != nil || !info.IsDir() {
		return "", false
	}
	return commit, true
}

// listHFTree lists every file under snapshots/<commit>, resolving each through
// the confinement (which follows the snapshot->blob symlink and rejects any that
// escapes entryRoot), and returns the (rfilename, size) pairs. An escaping or
// broken symlink is skipped, never advertised.
func listHFTree(entryRoot, commit string) []hfSibling {
	base, info, err := resolveConfinedTarget(entryRoot, "/snapshots/"+commit)
	if err != nil || !info.IsDir() {
		return nil
	}
	var out []hfSibling
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		resolved, fi, err := resolveConfinedTarget(entryRoot, "/snapshots/"+commit+"/"+relSlash)
		if err != nil || fi.IsDir() {
			return nil
		}
		_ = resolved
		out = append(out, hfSibling{RFilename: relSlash, Size: fi.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].RFilename < out[j].RFilename })
	return out
}

// --- GGUF (raw file) read surface -----------------------------------------

// handleCacheGGUF serves an EXACT indexed GGUF file:
//
//	GET|HEAD /cache/gguf/<cacheDir>/<path...>
//
// The requested path must exactly match (slash-normalized) an Entry.Files entry
// of a gguf-dir entry in that cache dir; anything unindexed is 404.
func (s *Server) handleCacheGGUF(w http.ResponseWriter, r *http.Request) {
	index, cacheRoot, ok := s.beginCacheRequest(w, r)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, cacheGGUFPrefix)
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	cacheDir := rest[:i]
	reqPath := rest[i+1:]
	if !isSingleSegment(cacheDir) || strings.TrimSpace(reqPath) == "" {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	rel := strings.TrimPrefix(path.Clean("/"+reqPath), "/")
	if !ggufFileIndexed(index, cacheDir, rel) {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	root, err := resolveConfinedRoot(filepath.Join(cacheRoot, cacheDir))
	if err != nil {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	resolved, info, err := resolveConfinedTarget(root, "/"+rel)
	if err != nil || info.IsDir() {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	serveConfinedFile(w, r, resolved, info)
}

// ggufFileIndexed reports whether rel is a recorded file of some gguf-dir entry
// in cacheDir. Matching is on the slash-normalized recorded path.
func ggufFileIndexed(index *cacheindex.Index, cacheDir, rel string) bool {
	for _, e := range index.All() {
		if e.Family != services.CacheFamilyGGUFDir || e.CacheDir != cacheDir {
			continue
		}
		for _, f := range e.Files {
			if strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(f)), "/") == rel {
				return true
			}
		}
	}
	return false
}

// --- Ollama (native manifest + referenced blobs) read surface -------------

// handleCacheOllama serves the indexed ollama manifest and ONLY the blobs it
// references:
//
//	GET /cache/ollama/manifest?model=<model>
//	GET|HEAD /cache/ollama/blob?model=<model>&digest=sha256:<hex>
//
// The model must be an indexed ollama entry. Ollama's model id is used as a query
// parameter (not a path segment) because it can contain both ':' and '/'.
//
// On-disk layout assumed (ollama's own):
//
//	<cacheRoot>/ollama/manifests/<host>/<namespace>/<name>/<tag>   (JSON)
//	<cacheRoot>/ollama/blobs/sha256-<hex>                          (blobs)
func (s *Server) handleCacheOllama(w http.ResponseWriter, r *http.Request) {
	index, cacheRoot, ok := s.beginCacheRequest(w, r)
	if !ok {
		return
	}
	action := strings.TrimPrefix(r.URL.Path, cacheOllamaPrefix)
	model := r.URL.Query().Get("model")

	if _, found := index.Lookup(ollamaCacheDir(), model); !found {
		exposeDeny(w, http.StatusNotFound, "model not found")
		return
	}
	manifestPath, ok := resolveOllamaManifest(cacheRoot, model)
	if !ok {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil || manifestInfo.IsDir() {
		exposeDeny(w, http.StatusNotFound, "not found")
		return
	}

	switch action {
	case "manifest":
		serveConfinedFile(w, r, manifestPath, manifestInfo)
	case "blob":
		digest := r.URL.Query().Get("digest")
		if !isSha256Digest(digest) {
			exposeDeny(w, http.StatusNotFound, "not found")
			return
		}
		refs, ok := ollamaManifestDigests(manifestPath)
		if !ok || !refs[digest] {
			// Not referenced by the indexed manifest: never served.
			exposeDeny(w, http.StatusNotFound, "not found")
			return
		}
		root, err := resolveConfinedRoot(filepath.Join(cacheRoot, ollamaCacheDir()))
		if err != nil {
			exposeDeny(w, http.StatusNotFound, "not found")
			return
		}
		blobFile := "sha256-" + strings.TrimPrefix(digest, "sha256:")
		resolved, info, err := resolveConfinedTarget(root, "/blobs/"+blobFile)
		if err != nil || info.IsDir() {
			exposeDeny(w, http.StatusNotFound, "not found")
			return
		}
		serveConfinedFile(w, r, resolved, info)
	default:
		exposeDeny(w, http.StatusNotFound, "not found")
	}
}

// ollamaCacheDir is the cache subdirectory name for the ollama native store.
func ollamaCacheDir() string {
	return services.EngineCacheDirs["ollama"].Dir
}

// resolveOllamaManifest derives the confined manifest path for an ollama model
// id and returns it if it exists on disk. It does NOT parse or serve — callers
// re-Stat / read as needed. ok=false on an unparseable model id, a cache dir
// that is not present, or a manifest that does not exist / escapes the store.
func resolveOllamaManifest(cacheRoot, model string) (string, bool) {
	host, ns, name, tag, ok := parseOllamaRef(model)
	if !ok {
		return "", false
	}
	root, err := resolveConfinedRoot(filepath.Join(cacheRoot, ollamaCacheDir()))
	if err != nil {
		return "", false
	}
	rel := "/manifests/" + host + "/" + ns + "/" + name + "/" + tag
	resolved, info, err := resolveConfinedTarget(root, rel)
	if err != nil || info.IsDir() {
		return "", false
	}
	return resolved, true
}

// parseOllamaRef splits an ollama model id ([host/][namespace/]name[:tag]) into
// its components with ollama's defaults (host registry.ollama.ai, namespace
// library, tag latest). Every component must be a single, safe path segment.
func parseOllamaRef(model string) (host, ns, name, tag string, ok bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", "", "", "", false
	}
	host, ns, tag = "registry.ollama.ai", "library", "latest"
	ref := model
	// A tag is the part after the LAST ':' only when it comes after the last '/'
	// (so a host:port form is not mistaken for a tag — ollama hosts have no port
	// in this layout, but guard anyway).
	if i := strings.LastIndexByte(ref, ':'); i >= 0 && i > strings.LastIndexByte(ref, '/') {
		tag = ref[i+1:]
		ref = ref[:i]
	}
	segs := strings.Split(ref, "/")
	switch len(segs) {
	case 1:
		name = segs[0]
	case 2:
		ns, name = segs[0], segs[1]
	case 3:
		host, ns, name = segs[0], segs[1], segs[2]
	default:
		return "", "", "", "", false
	}
	for _, seg := range []string{host, ns, name, tag} {
		if !isSingleSegment(seg) {
			return "", "", "", "", false
		}
	}
	return host, ns, name, tag, true
}

// ollamaManifestDigests reads an ollama manifest and returns the set of digests
// it references (config.digest + every layers[].digest). These are the ONLY
// blobs cache serving will hand out for that model.
func ollamaManifestDigests(manifestPath string) (map[string]bool, bool) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, false
	}
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	set := map[string]bool{}
	if m.Config.Digest != "" {
		set[m.Config.Digest] = true
	}
	for _, l := range m.Layers {
		if l.Digest != "" {
			set[l.Digest] = true
		}
	}
	return set, true
}

// --- Shared helpers -------------------------------------------------------

// writeCacheJSON writes v as JSON, suppressing the body for a HEAD request.
func writeCacheJSON(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		exposeDeny(w, http.StatusInternalServerError, "encode error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// isSingleSegment reports whether s is a single, safe path segment: non-empty,
// no path separator, and not "." or ".." (which would traverse).
func isSingleSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/\\") && !strings.ContainsRune(s, 0)
}

// isHexCommit reports whether s is a plausible git/HF commit hash (hex, 7..64
// chars).
func isHexCommit(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	return isHex(s)
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return len(s) > 0
}

// isSha256Digest reports whether s is exactly "sha256:" + 64 hex chars.
func isSha256Digest(s string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	hex := s[len(prefix):]
	return len(hex) == 64 && isHex(hex)
}
