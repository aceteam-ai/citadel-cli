package catalog

import (
	"errors"
	"strings"
	"testing"
)

// stubRefresh swaps the package-level catalogRefresh for the duration of a test,
// returning a pointer to a call counter so a test can assert the refresh ran the
// expected number of times (bounded == exactly once). It restores the real
// Update on cleanup.
func stubRefresh(t *testing.T, fn func() error) *int {
	t.Helper()
	var calls int
	prev := catalogRefresh
	catalogRefresh = func() error {
		calls++
		return fn()
	}
	t.Cleanup(func() { catalogRefresh = prev })
	return &calls
}

// TestResolveSelfHealingColdCacheRefreshesOnce verifies the core self-heal: a
// module absent from a cold cache resolves after exactly one automatic refresh
// (the refresh seeds the cache), and the refresh is not called a second time.
func TestResolveSelfHealingColdCacheRefreshesOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	calls := stubRefresh(t, func() error {
		// The "refresh" seeds the default catalog cache, as `catalog update` would.
		seedService(t, GetCatalogPath(), "meeting", "name: meeting\nversion: 1.0.0\n")
		return nil
	})

	resolved, err := ResolveCatalogServiceSelfHealing("meeting")
	if err != nil {
		t.Fatalf("ResolveCatalogServiceSelfHealing: %v", err)
	}
	if resolved.SourceName != DefaultSourceName {
		t.Errorf("resolved source = %q, want %q", resolved.SourceName, DefaultSourceName)
	}
	if *calls != 1 {
		t.Errorf("catalog refresh called %d times, want exactly 1 (bounded, no retry storm)", *calls)
	}
}

// TestResolveSelfHealingStillAbsentAfterRefresh verifies that a module still
// missing after the single refresh returns the clear not-found error, and the
// refresh ran exactly once (no storm).
func TestResolveSelfHealingStillAbsentAfterRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	calls := stubRefresh(t, func() error { return nil }) // refresh seeds nothing

	_, err := ResolveCatalogServiceSelfHealing("nonexistent")
	if err == nil {
		t.Fatal("expected an error for a module absent after refresh")
	}
	if !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("error = %v, want it to wrap ErrServiceNotFound", err)
	}
	if !strings.Contains(err.Error(), "not found in catalog") {
		t.Errorf("error = %q, want the clear 'not found in catalog' message", err.Error())
	}
	if *calls != 1 {
		t.Errorf("catalog refresh called %d times, want exactly 1", *calls)
	}
}

// TestResolveSelfHealingNoCacheAndRefreshFailed: with no catalog cache at all
// and a failed refresh, the catalog cannot answer, so the error must say the
// catalog is unavailable (not "not found") and carry the sanitized cause.
func TestResolveSelfHealingNoCacheAndRefreshFailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	calls := stubRefresh(t, func() error {
		return errors.New("clone failed: fatal: unable to access\n'https://user:tok@example.invalid/x/': no such host")
	})

	_, err := ResolveCatalogServiceSelfHealing("meeting")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Errorf("error = %v, want ErrCatalogUnavailable", err)
	}
	if errors.Is(err, ErrServiceNotFound) || strings.Contains(err.Error(), "not found in catalog") {
		t.Errorf("error = %q, must not claim the service is not found", err.Error())
	}
	if !strings.Contains(err.Error(), "citadel service catalog update") {
		t.Errorf("error = %q, want the catalog update hint", err.Error())
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Errorf("error = %q, want the cause", err.Error())
	}
	if strings.Contains(err.Error(), "tok@") || strings.Contains(err.Error(), "\n") {
		t.Errorf("error = %q, cause not sanitized", err.Error())
	}
	if *calls != 1 {
		t.Errorf("catalog refresh called %d times, want exactly 1", *calls)
	}
}

// TestLoadManifestSelfHealingNoCacheAndRefreshFailed covers the manifest path
// used by `catalog info` / `module info`.
func TestLoadManifestSelfHealingNoCacheAndRefreshFailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubRefresh(t, func() error { return errors.New("offline") })

	_, err := LoadServiceManifestSelfHealing("meeting")
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("error = %v, want ErrCatalogUnavailable", err)
	}
}

// TestResolveSelfHealingPresentCacheLacksNameRefreshFailed: a present catalog
// that lacks the name still reports not-found, plus the refresh failure.
func TestResolveSelfHealingPresentCacheLacksNameRefreshFailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedService(t, GetCatalogPath(), "other", "name: other\nversion: 1.0.0\n")
	stubRefresh(t, func() error { return errors.New("no such host") })

	_, err := ResolveCatalogServiceSelfHealing("nonexistent")
	if !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("error = %v, want ErrServiceNotFound", err)
	}
	if !strings.Contains(err.Error(), "catalog refresh also failed") || !strings.Contains(err.Error(), "no such host") {
		t.Errorf("error = %q, want the refresh failure surfaced", err.Error())
	}
}

// TestResolveSelfHealingRefreshPartialErrorButResolves is the landmine guard: a
// node with a broken community source makes Update() return a non-nil (partial)
// error even though the DEFAULT catalog refreshed fine. The wrapper must retry
// resolution first and succeed -- it must not refuse the install just because the
// refresh returned an error. This mirrors #514 (meeting is a default module).
func TestResolveSelfHealingRefreshPartialErrorButResolves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	calls := stubRefresh(t, func() error {
		// The default catalog refreshed fine (module now present)...
		seedService(t, GetCatalogPath(), "meeting", "name: meeting\nversion: 1.0.0\n")
		// ...but a broken community source made Update() return a partial error.
		return errors.New("catalog source \"evilcat\": git clone failed")
	})

	resolved, err := ResolveCatalogServiceSelfHealing("meeting")
	if err != nil {
		t.Fatalf("expected success despite partial refresh error, got: %v", err)
	}
	if resolved.SourceName != DefaultSourceName {
		t.Errorf("resolved source = %q, want %q", resolved.SourceName, DefaultSourceName)
	}
	if *calls != 1 {
		t.Errorf("catalog refresh called %d times, want exactly 1", *calls)
	}
}

// TestResolveSelfHealingWarmCacheNoRefresh verifies the common case: a module
// already present in the cache resolves WITHOUT any refresh (zero network cost).
func TestResolveSelfHealingWarmCacheNoRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedService(t, GetCatalogPath(), "meeting", "name: meeting\nversion: 1.0.0\n")

	calls := stubRefresh(t, func() error {
		t.Error("refresh must not run when the module is already in the cache")
		return nil
	})

	if _, err := ResolveCatalogServiceSelfHealing("meeting"); err != nil {
		t.Fatalf("ResolveCatalogServiceSelfHealing: %v", err)
	}
	if *calls != 0 {
		t.Errorf("catalog refresh called %d times, want 0 on a warm cache", *calls)
	}
}

// TestLoadServiceManifestSelfHealingColdCache verifies the reconcile / MODULE_SET
// catalog path (resolveModuleForTUI -> LoadServiceManifestSelfHealing) also
// self-heals a cold cache with exactly one refresh.
func TestLoadServiceManifestSelfHealingColdCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	calls := stubRefresh(t, func() error {
		seedService(t, GetCatalogPath(), "meeting", "name: meeting\nversion: 1.0.0\n")
		return nil
	})

	manifest, err := LoadServiceManifestSelfHealing("meeting")
	if err != nil {
		t.Fatalf("LoadServiceManifestSelfHealing: %v", err)
	}
	if manifest.Name != "meeting" {
		t.Errorf("manifest name = %q, want meeting", manifest.Name)
	}
	if *calls != 1 {
		t.Errorf("catalog refresh called %d times, want exactly 1", *calls)
	}
}
