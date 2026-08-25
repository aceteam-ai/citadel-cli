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
)

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
	if refreshErr != nil {
		return nil, fmt.Errorf("%w; catalog refresh also failed: %v", err, refreshErr)
	}
	return nil, err
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
	if refreshErr != nil {
		return nil, fmt.Errorf("%w; catalog refresh also failed: %v", err, refreshErr)
	}
	return nil, err
}
