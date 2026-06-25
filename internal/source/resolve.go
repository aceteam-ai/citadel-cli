// Package source resolves a control-plane-assigned module source string into an
// installable descriptor and supplies git credentials for PRIVATE sources, for
// the remote-managed-node path (issue #354, part of epic #352).
//
// It is the node-side companion to internal/reconcile: a reconcile
// ModuleAssignment.Source is an opaque string (catalog name | owner/repo[@ref] |
// git URL); this package classifies it (REUSING internal/catalog's parser — it
// is not re-implemented here), then, for a private source, selects the git
// credential the node should clone with.
//
// Dependency direction (deliberate, no cycles):
//
//	internal/reconcile  ->  internal/source  ->  internal/catalog
//
// reconcile stays pure and does NOT import this package or catalog; the LIVE
// ModuleOps adapter (a later increment, see internal/reconcile/ops.go) is what
// wires reconcile to this resolution+credential path and to the actual clone.
//
// SECURITY POSTURE (hard requirements, see credentials.go):
//   - Credentials are NEVER logged, committed, or placed in error messages /
//     job payloads.
//   - The resolution Descriptor produced here is log-safe: it carries the
//     PLAIN clone URL and never an authenticated (token-bearing) URL. The
//     authenticated URL is produced only at apply time (apply.go) and is itself
//     treated as a secret.
//   - File-backed credentials are stored 0600 and the mode is enforced on READ.
package source

import (
	"fmt"

	"github.com/aceteam-ai/citadel-cli/internal/catalog"
)

// Kind classifies how a source string is resolved. It mirrors
// catalog.SourceKind so callers of this package do not need to import catalog
// just to switch on the kind.
type Kind int

const (
	// KindCatalog is a bare catalog service name (resolved from the central
	// catalog; it has no clone URL of its own).
	KindCatalog Kind = iota
	// KindGitHub is an "owner/repo[@ref]" shorthand (clones from github.com).
	KindGitHub
	// KindGitURL is a full git URL (scheme or scp form).
	KindGitURL
)

// String renders a Kind for diagnostics.
func (k Kind) String() string {
	switch k {
	case KindCatalog:
		return "catalog"
	case KindGitHub:
		return "github"
	case KindGitURL:
		return "git-url"
	default:
		return "unknown"
	}
}

// Descriptor is a normalized, LOG-SAFE resolution of an assigned source string:
// what kind it is, where to clone it from, and at which ref. It intentionally
// carries only non-secret data — never a token, never an authenticated URL — so
// it can be logged, reported back to the control plane, or embedded in an error
// without leaking a credential. Authentication is applied separately and only at
// clone time (see ApplyCredential).
type Descriptor struct {
	// Kind is the resolution strategy.
	Kind Kind
	// Raw is the original assigned source string.
	Raw string
	// CloneURL is the PLAIN (unauthenticated) git clone URL for KindGitHub /
	// KindGitURL. Empty for KindCatalog.
	CloneURL string
	// Ref is the requested tag/branch/sha/constraint (empty = default branch).
	Ref string
	// Host is the bare host the source clones from (e.g. "github.com"), used as
	// the credential lookup key. Empty for KindCatalog.
	Host string

	// catalogSrc is the underlying parsed catalog.Source, retained so the live
	// install adapter can hand it straight to catalog.ResolveSource without
	// re-parsing. Unexported: it is an internal detail, not part of the
	// log-safe surface.
	catalogSrc catalog.Source
}

// CatalogSource returns the underlying parsed catalog.Source. The live module
// adapter passes this to catalog.ResolveSource for the actual clone/load, so the
// classification is parsed exactly once.
func (d Descriptor) CatalogSource() catalog.Source { return d.catalogSrc }

// IsCatalog reports whether the source is a bare catalog name (and so is
// installed via the existing catalog path rather than a git clone).
func (d Descriptor) IsCatalog() bool { return d.Kind == KindCatalog }

// Resolve classifies an assigned source string into a log-safe Descriptor by
// delegating to catalog.ParseSource (the single source of truth for the
// catalog | owner/repo@ref | git-URL grammar, including the scp/userinfo/ref
// edge cases). It does NOT clone or touch the network.
func Resolve(rawSource string) (Descriptor, error) {
	src, err := catalog.ParseSource(rawSource)
	if err != nil {
		return Descriptor{}, fmt.Errorf("resolve source: %w", err)
	}

	d := Descriptor{
		Raw:        src.Raw,
		CloneURL:   src.CloneURL,
		Ref:        src.Ref,
		catalogSrc: src,
	}
	switch src.Kind {
	case catalog.KindCatalog:
		d.Kind = KindCatalog
	case catalog.KindGitHub:
		d.Kind = KindGitHub
	case catalog.KindGitURL:
		d.Kind = KindGitURL
	default:
		return Descriptor{}, fmt.Errorf("resolve source %q: unknown source kind", rawSource)
	}

	// A catalog name has no clone host; everything else is keyed by its host.
	if d.Kind != KindCatalog {
		d.Host = catalog.SourceHost(src)
	}
	return d, nil
}
