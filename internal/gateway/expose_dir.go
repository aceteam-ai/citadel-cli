// internal/gateway/expose_dir.go
//
// Directory-source exposures (issue #943): a SECOND source type for `expose`
// alongside the port-proxy source in exposure.go. Instead of reverse-proxying
// to a local TCP address, ExposeDir serves a workspace-confined, read-only
// directory tree directly from this package under the same /expose/<name>/
// namespace, the same exposureMiddleware sole-gate, and the same durable
// ExposureRecord restore-on-restart (#647) — no customer-run web server.
//
// # Confinement is the acceptance bar, not http.Dir
//
// A bare http.FileServer(http.Dir(root)) is NOT safe to serve here: os.Open
// follows symlinks, so a symlink planted inside the exposed tree that points
// outside root would let a request walk out of the confined directory even
// though the LEXICAL request path never contains "..". resolveConfinedTarget
// closes that: it EvalSymlinks the request target on EVERY request (mirroring
// internal/jobs.ValidatePath's posture for FILE_READ/FILE_LIST) and rejects
// anything whose resolved location escapes the resolved root. The root itself
// is resolved once, at ExposeDir time, so a caller who mutates the tree after
// exposing it cannot widen what a later request can reach.
//
// This package deliberately does NOT import internal/jobs to reuse
// ValidatePath verbatim (mirroring the "standalone by design" posture the rest
// of this package documents for internal/network) — the algorithm is
// duplicated locally instead. The confined ROOT this file is handed is
// expected to already be workspace-validated by the caller (the cmd-layer
// ExposeOps adapter, via internal/jobs.ValidatePath against the node's
// resolveWorkspaceDir()) — deliberately NOT the AllowReadOutsideWorkspace
// relaxation FILE_READ/FILE_LIST can opt into: a network-reachable share is
// workspace-pinned regardless of that flag. This file's own confinement is a
// second, independent layer that holds even if a future caller forgets that
// validation, since it re-derives and re-checks the boundary on every request
// rather than trusting the root it was given was clean.
package gateway

import (
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// dirRoot holds the confined, symlink-resolved filesystem root backing one
// directory-source exposure. Mutable (like Upstream.dynAddr) so a re-Expose or
// Unexpose can re-point or clear the live target without re-registering the
// mux handler — http.ServeMux has no deregister, so the handler closure must
// read the CURRENT root on every request rather than closing over a fixed one.
type dirRoot struct {
	mu   sync.RWMutex
	root string // resolved, absolute; "" means not currently serving.
}

func (d *dirRoot) get() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.root
}

func (d *dirRoot) set(root string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.root = root
}

// ExposeDir wires the /expose/<name> route to serve rootDir as a confined,
// read-only, auto-indexed static directory, and records its visibility policy
// — the directory-source analogue of Expose. rootDir must be an absolute path
// to an existing directory; the caller (the cmd-layer ExposeOps adapter) is
// responsible for confining rootDir to the node's workspace before calling
// this, since this package does not know what "the workspace" is. This
// function additionally resolves rootDir's own symlinks once, so the
// confinement boundary used on every subsequent request is already
// symlink-free.
func (s *Server) ExposeDir(name, rootDir string, policy *ExposePolicy) error {
	if name == "" {
		return fmt.Errorf("gateway: ExposeDir requires a non-empty service name")
	}
	if !exposeNamePattern.MatchString(name) {
		return fmt.Errorf("gateway: expose name %q must be lowercase alphanumeric and dashes only", name)
	}
	if policy == nil || !policy.Visibility.Valid() {
		return fmt.Errorf("gateway: ExposeDir requires a valid visibility (private|org|link)")
	}
	// Mirror the cross-type guard in Expose: a name already claimed by a proxy
	// upstream cannot become a directory share (http.ServeMux has no
	// deregister — see the dirExposures doc comment on Server).
	s.mu.RLock()
	_, isProxy := s.config.Upstreams[ExposeRoutePath(name)]
	s.mu.RUnlock()
	if isProxy {
		return fmt.Errorf("gateway: expose %q is already registered as a port source; use a different name to expose a directory", name)
	}

	resolvedRoot, err := resolveConfinedRoot(rootDir)
	if err != nil {
		return fmt.Errorf("gateway: ExposeDir: %w", err)
	}

	s.wireExposeDirRoute(ExposeRoutePath(name), resolvedRoot)
	s.SetExposure(name, policy)
	return nil
}

// resolveConfinedRoot validates and resolves the root directory for a
// directory-source exposure: it must be an absolute, existing directory, and
// its own symlinks are resolved once here so every later per-request check
// compares against an already-clean boundary.
func resolveConfinedRoot(rootDir string) (string, error) {
	if rootDir == "" {
		return "", fmt.Errorf("directory source requires a non-empty path")
	}
	if !filepath.IsAbs(rootDir) {
		return "", fmt.Errorf("directory source path %q must be absolute", rootDir)
	}
	resolved, err := filepath.EvalSymlinks(rootDir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve directory %q: %w", rootDir, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("cannot stat directory %q: %w", rootDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %q is not a directory", rootDir)
	}
	return filepath.Clean(resolved), nil
}

// wireExposeDirRoute registers (or re-points) the /expose/<name> directory
// handler. Mirrors wireExposeRoute's re-wire-safe semantics exactly: a new
// route created after Start registers its handler live; an existing route has
// its root mutated in place (never replaced), so the running handler keeps
// reading the current target.
func (s *Server) wireExposeDirRoute(prefix, root string) {
	s.mu.Lock()
	dr, ok := s.dirExposures[prefix]
	if !ok {
		if s.dirExposures == nil {
			s.dirExposures = make(map[string]*dirRoot)
		}
		dr = &dirRoot{}
		dr.set(root)
		s.dirExposures[prefix] = dr
		if s.started {
			s.registerDirHandler(prefix, dr)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	dr.set(root)
}

// registerDirHandler registers the confined static-file handler for prefix on
// the mux, both with and without a trailing slash (mirrors registerProxy).
func (s *Server) registerDirHandler(prefix string, dr *dirRoot) {
	handler := s.confinedDirHandler(prefix, dr)
	s.mux.HandleFunc(prefix+"/", handler)
	s.mux.HandleFunc(prefix, handler)
}

// confinedDirHandler returns the per-request handler for a directory-source
// exposure at prefix, reading dr's current root on every call (never a
// closed-over snapshot) so Unexpose/re-Expose take effect immediately.
func (s *Server) confinedDirHandler(prefix string, dr *dirRoot) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		root := dr.get()
		if root == "" {
			// Torn down (Unexpose) but the mux pattern (and possibly the policy,
			// during a narrow race) still exists. exposureMiddleware is the SOLE
			// gate and already 404s once the policy is removed; this only covers
			// the brief window it doesn't.
			exposeDeny(w, http.StatusBadGateway, "exposure not available")
			return
		}

		rest := strings.TrimPrefix(r.URL.Path, prefix)
		if rest == "" {
			rest = "/"
		}
		if !strings.HasPrefix(rest, "/") {
			rest = "/" + rest
		}
		cleanRel := path.Clean(rest)
		if !strings.HasPrefix(cleanRel, "/") {
			cleanRel = "/" + cleanRel
		}

		resolved, info, err := resolveConfinedTarget(root, cleanRel)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			exposeDeny(w, http.StatusForbidden, "access denied")
			return
		}

		if info.IsDir() {
			// Redirect a bare directory path to its trailing-slash form so
			// relative links in the auto-index (and in index.html, if present)
			// resolve against the right base — mirrors http.FileServer.
			if !strings.HasSuffix(r.URL.Path, "/") {
				target := r.URL.Path + "/"
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
				return
			}
			serveConfinedDir(w, r, root, resolved, cleanRel)
			return
		}

		serveConfinedFile(w, r, resolved, info)
	}
}

// resolveConfinedTarget resolves cleanRel (a Cleaned, "/"-rooted URL path)
// against root and returns the resolved, existing filesystem target — but
// ONLY if it stays within root after symlink resolution. This is the check
// that closes the http.Dir landmine: a symlink inside root whose target
// escapes it is rejected here even though root+cleanRel never lexically
// contains "..".
func resolveConfinedTarget(root, cleanRel string) (string, os.FileInfo, error) {
	rel := strings.TrimPrefix(cleanRel, "/")
	target := filepath.Join(root, filepath.FromSlash(rel))

	// Lexical belt-and-suspenders: path.Clean on a rooted ("/"-prefixed) input
	// can never leave a leading "..", so this should always hold — but assert
	// it before ever touching the filesystem, mirroring ValidatePath's
	// posture of never trusting a single check alone.
	if !withinDir(root, target) {
		return "", nil, os.ErrNotExist
	}

	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", nil, err
	}
	if !withinDir(root, resolved) {
		// A symlink inside the exposed tree resolves outside the confined
		// root — the escape this handler exists to close. Report not-found
		// rather than confirming the traversal succeeded.
		return "", nil, os.ErrNotExist
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, err
	}
	return resolved, info, nil
}

// withinDir reports whether target is dir itself or (after symlink
// resolution, by the caller) lexically beneath it. Uses filepath.Rel rather
// than a string prefix so /root does not match /rootEVIL.
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// serveConfinedFile serves one already-confined, already-resolved file.
func serveConfinedFile(w http.ResponseWriter, r *http.Request, resolved string, info os.FileInfo) {
	f, err := os.Open(resolved)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, filepath.Base(resolved), info.ModTime(), f)
}

// serveConfinedDir serves one already-confined, already-resolved directory:
// its index.html if present, else a rendered auto-index listing (issue #943 —
// nothing like this existed before). The index.html check re-confines through
// resolveConfinedTarget rather than assuming a name found inside an
// already-confined directory is itself safe: index.html could be a symlink
// escaping root even though the directory containing it is not.
func serveConfinedDir(w http.ResponseWriter, r *http.Request, root, resolvedDir, cleanRel string) {
	indexRel := strings.TrimSuffix(cleanRel, "/") + "/index.html"
	if resolvedIndex, info, err := resolveConfinedTarget(root, indexRel); err == nil && !info.IsDir() {
		serveConfinedFile(w, r, resolvedIndex, info)
		return
	}
	renderDirIndex(w, r, root, resolvedDir, cleanRel)
}

// dirIndexView is the data rendered by dirIndexTemplate.
type dirIndexView struct {
	Path      string
	HasParent bool
	Entries   []dirIndexEntry
}

// dirIndexEntry is one row of an auto-rendered directory listing.
type dirIndexEntry struct {
	Name     string
	Href     string
	Size     string
	Modified string
}

// dirIndexTemplate renders a minimal auto-index listing (name, size,
// modified) for a directory with no index.html. html/template auto-escapes
// entry names, so a maliciously-named file cannot inject markup into the
// listing.
var dirIndexTemplate = template.Must(template.New("dirIndex").Parse(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Index of {{.Path}}</title></head>
<body>
<h1>Index of {{.Path}}</h1>
<table>
<tr><th align="left">Name</th><th align="left">Size</th><th align="left">Modified</th></tr>
{{if .HasParent}}<tr><td><a href="../">../</a></td><td>-</td><td>-</td></tr>{{end}}
{{range .Entries}}<tr><td><a href="{{.Href}}">{{.Name}}</a></td><td>{{.Size}}</td><td>{{.Modified}}</td></tr>
{{end}}</table>
</body>
</html>
`))

// renderDirIndex writes the auto-index listing for resolvedDir (already
// confined and resolved by the caller, and itself beneath root).
func renderDirIndex(w http.ResponseWriter, r *http.Request, root, resolvedDir, cleanRel string) {
	entries, err := os.ReadDir(resolvedDir)
	if err != nil {
		exposeDeny(w, http.StatusForbidden, "cannot list directory")
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	view := dirIndexView{Path: cleanRel, HasParent: cleanRel != "/"}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()

		// A symlink inside the exposed tree that resolves OUTSIDE root is
		// already blocked from being SERVED (resolveConfinedTarget, on
		// click) -- but advertising its name here is still a leak (confirms
		// the target exists) and a confusing dead link. Resolve every entry
		// (a no-op for a plain file/dir, since resolvedDir is already
		// confined and entry names never contain a path separator) and skip
		// it if it escapes root, mirroring resolveConfinedTarget's own
		// confinement check. A broken symlink (EvalSymlinks errors) is
		// skipped the same way -- it would 404 on click regardless.
		entryPath := filepath.Join(resolvedDir, name)
		resolvedEntry, err := filepath.EvalSymlinks(entryPath)
		if err != nil || !withinDir(root, resolvedEntry) {
			continue
		}

		href := name
		size := humanSize(info.Size())
		if e.IsDir() {
			href += "/"
			size = "-"
		}
		view.Entries = append(view.Entries, dirIndexEntry{
			Name:     name,
			Href:     href,
			Size:     size,
			Modified: info.ModTime().UTC().Format("2006-01-02 15:04:05 UTC"),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_ = dirIndexTemplate.Execute(w, view)
}

// humanSize formats a byte count for the auto-index listing.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
