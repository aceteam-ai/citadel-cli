// internal/catalog/selfheal.go
//
// Self-healing catalog resolution (aceteam-ai/aceteam#518).
//
// The install path resolves a module from the node's LOCAL catalog cache. On a
// COLD cache (a fresh node that never ran `citadel service catalog update`) that
// resolution fails with "service '<name>' not found in catalog" -- exactly what
// bit the meeting-module (#514) prod canary. Over the remote MODULE_SET /
// reconcile-pull converge path there is no operator to run `catalog update`
// manually, so a desired-state assignment could never install on a cold node.
//
// These wrappers make resolution self-healing: when a service is absent from the
// local cache, refresh the configured catalog source(s) ONCE, then retry before
// failing. Bounded to a single refresh per resolution (no retry storm); a service
// still absent after the refresh returns the same clear error.
package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrCatalogUnavailable is returned when no catalog cache exists and the
// automatic refresh failed, so the catalog cannot say whether a name exists.
// It deliberately does not wrap ErrServiceNotFound: "not found" is only
// reported when a present or freshly fetched catalog really lacks the name.
var ErrCatalogUnavailable = errors.New("catalog not available; run `citadel service catalog update`")

var urlUserinfoPattern = regexp.MustCompile(`(://)[^/\s]+@`)

var secretQueryPattern = regexp.MustCompile(`(?i)\b((?:access_|refresh_|id_|auth_)?token|password|passwd|secret|api_?key|key|sig|signature)=[^&\s'"]+`)

// sanitizeRefreshCause makes a catalog refresh error safe and short for an
// operator-facing message: one line, URL credentials removed, bounded length.
func sanitizeRefreshCause(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	s = urlUserinfoPattern.ReplaceAllString(s, "$1")
	s = secretQueryPattern.ReplaceAllString(s, "$1=REDACTED")
	const max = 200
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "..."
	}
	return s
}

// anyCatalogCached reports whether at least one configured source has a cache.
func anyCatalogCached() bool {
	sources, err := resolvedSources()
	if err != nil {
		return false
	}
	for _, src := range sources {
		if dirIsAvailable(src.Path) {
			return true
		}
	}
	return false
}

// afterFailedLookup builds the error for a lookup that is still unresolved after
// the one refresh.
func afterFailedLookup(lookupErr, refreshErr error) error {
	if refreshErr == nil {
		return lookupErr
	}
	cause := sanitizeRefreshCause(refreshErr)
	if !anyCatalogCached() {
		return fmt.Errorf("%w: %s", ErrCatalogUnavailable, cause)
	}
	return fmt.Errorf("%w; catalog refresh also failed: %s", lookupErr, cause)
}

// ErrServiceNotFound is the sentinel wrapped by the catalog resolvers when a
// service name is absent from every configured source's LOCAL cache. The
// self-healing wrappers key their single catalog refresh on this sentinel: a
// not-found triggers exactly one refresh-and-retry, while any OTHER resolution
// error (unparseable manifest, stat failure) is returned immediately without a
// refresh.
var ErrServiceNotFound = errors.New("service not found in catalog")

// catalogRefresh is the refresh the self-healing resolvers run when a service is
// missing from the local cache. It is a package var so tests can substitute a
// fake refresh (seeding a temp cache) instead of a real git clone/pull. In
// production it is Update -- clone/pull of every configured source.
var catalogRefresh = Update

// ResolveCatalogServiceSelfHealing resolves a catalog service name atomically
// (see ResolveCatalogService for the single-source shadow-safety invariant) and,
// when the service is absent from the local cache, refreshes the configured
// catalog source(s) exactly ONCE and retries before failing.
//
// The refresh error is deliberately NOT treated as fatal on its own: Update() is
// partial-failure-tolerant (it refreshes every source and returns the first error
// even if the rest, including the built-in default, succeeded). So a node with a
// broken community source added must still be able to install a default-catalog
// module (e.g. `meeting`). We retry resolution first and only surface the refresh
// error if the service is STILL absent afterward.
func ResolveCatalogServiceSelfHealing(name string) (*ResolvedCatalogService, error) {
	resolved, err := ResolveCatalogService(name)
	if err == nil || !errors.Is(err, ErrServiceNotFound) {
		return resolved, err
	}

	refreshErr := catalogRefresh()

	resolved, err = ResolveCatalogService(name)
	if err == nil {
		return resolved, nil
	}
	return nil, afterFailedLookup(err, refreshErr)
}

// LoadServiceManifestSelfHealing is LoadServiceManifest with the same
// refresh-once-on-cold-cache behavior, for the reconcile / MODULE_SET catalog
// path (resolveModuleForTUI). After a successful refresh the cache is warm, so
// the caller's follow-up GetComposeFile read also succeeds -- one refresh covers
// both manifest and compose.
func LoadServiceManifestSelfHealing(name string) (*ServiceManifest, error) {
	manifest, err := LoadServiceManifest(name)
	if err == nil || !errors.Is(err, ErrServiceNotFound) {
		return manifest, err
	}

	refreshErr := catalogRefresh()

	manifest, err = LoadServiceManifest(name)
	if err == nil {
		return manifest, nil
	}
	return nil, afterFailedLookup(err, refreshErr)
}
