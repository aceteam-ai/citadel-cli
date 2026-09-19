package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func mustBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestApplyRoutesResponse_ReplaceKeepDrop(t *testing.T) {
	now := time.Unix(1000, 0)
	prev := &RouteMap{etag: "old", routes: map[string]Route{"old": {Slug: "old"}}, fetchedAt: time.Unix(500, 0)}

	// 304 re-stamps last-good fresh: a NEW map sharing prev's routes/etag with
	// fetchedAt advanced to now (so a healthy 304 feed does not expire).
	got, err := applyRoutesResponse(prev, 304, "", nil, now)
	if err != nil {
		t.Fatalf("304: unexpected err %v", err)
	}
	if got == prev {
		t.Fatal("304: expected a re-stamped clone, got the same pointer")
	}
	if got.ETag() != "old" || got.routes["old"].Slug != "old" {
		t.Fatalf("304: content should be preserved, got etag=%q routes=%v", got.ETag(), got.routes)
	}
	if !got.fetchedAt.Equal(now) {
		t.Fatalf("304: fetchedAt = %v, want %v (freshness reset)", got.fetchedAt, now)
	}

	// 5xx keeps last-good UNCHANGED (no re-stamp) and returns an error.
	got, err = applyRoutesResponse(prev, 503, "", []byte("nope"), now)
	if err == nil || got != prev {
		t.Fatalf("5xx: got=%v err=%v, want prev + error", got, err)
	}

	// Malformed JSON keeps last-good and returns an error.
	got, err = applyRoutesResponse(prev, 200, "e1", []byte("{"), now)
	if err == nil || got != prev {
		t.Fatalf("malformed: got=%v err=%v, want prev + error", got, err)
	}

	// 200 replaces; a valid entry is kept, one missing mesh_ip is DROPPED, one
	// non-mesh IP is REJECTED -- but the rest of the map still applies.
	body := mustBody(t, routesPayload{
		Routes: map[string]routeEntry{
			"good":    {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
			"nomesh":  {MeshIP: "", Port: 8080, Visibility: "public"},
			"rfc1918": {MeshIP: "10.0.0.5", Port: 8080, Visibility: "public"},
			"badport": {MeshIP: "100.64.0.9", Port: 0, Visibility: "public"},
			"gated":   {MeshIP: "100.100.0.2", Port: 9000, Visibility: "gated"},
		},
		LoginURL:       "https://login.example.com",
		SessionCookies: []string{"sess"},
	})
	got, err = applyRoutesResponse(prev, 200, "e2", body, now)
	if err != nil {
		t.Fatalf("200: unexpected err %v", err)
	}
	if !got.fetchedAt.Equal(now) {
		t.Fatalf("200: fetchedAt = %v, want %v", got.fetchedAt, now)
	}
	if got == prev {
		t.Fatal("200: expected a new map, got prev")
	}
	if got.ETag() != "e2" {
		t.Fatalf("etag = %q, want e2", got.ETag())
	}
	if _, ok := got.routes["good"]; !ok {
		t.Error("good entry missing")
	}
	if got.routes["gated"].Visibility != VisibilityGated {
		t.Errorf("gated visibility = %q, want gated", got.routes["gated"].Visibility)
	}
	for _, bad := range []string{"nomesh", "rfc1918", "badport"} {
		if _, ok := got.routes[bad]; ok {
			t.Errorf("invalid entry %q was not dropped", bad)
		}
	}
	if len(got.routes) != 2 {
		t.Fatalf("route count = %d, want 2 (good, gated)", len(got.routes))
	}
	if got.LoginURL() != "https://login.example.com" || len(got.CookieNames()) != 1 {
		t.Errorf("payload fields not carried: login=%q cookies=%v", got.LoginURL(), got.CookieNames())
	}
}

// fakeSource is an injectable RoutesSource driven by function fields.
type fakeSource struct {
	fetchFn       func(ctx context.Context, etag string) (int, string, []byte, error)
	fetchOneFn    func(ctx context.Context, slug string) (int, []byte, error)
	fetchOneCalls int32
}

func (f *fakeSource) Fetch(ctx context.Context, etag string) (int, string, []byte, error) {
	return f.fetchFn(ctx, etag)
}

func (f *fakeSource) FetchOne(ctx context.Context, slug string) (int, []byte, error) {
	atomic.AddInt32(&f.fetchOneCalls, 1)
	return f.fetchOneFn(ctx, slug)
}

func TestClientResolve_OnMissThenNegativeCacheSuppresses(t *testing.T) {
	// Full map has "known"; "unknown" is not in it and FetchOne 404s.
	full := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"known": {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
	}})
	src := &fakeSource{
		fetchFn: func(ctx context.Context, etag string) (int, string, []byte, error) {
			return 200, "e1", full, nil
		},
		fetchOneFn: func(ctx context.Context, slug string) (int, []byte, error) {
			return 404, nil, nil // unknown
		},
	}
	c := NewClient(ClientConfig{Source: src, NegativeTTL: time.Minute})
	c.pollOnce(context.Background())

	if _, ok := c.Resolve(context.Background(), "known"); !ok {
		t.Fatal("known slug should resolve from the map")
	}
	if atomic.LoadInt32(&src.fetchOneCalls) != 0 {
		t.Fatal("a map hit must not trigger an on-miss lookup")
	}

	// First unknown lookup -> one on-miss FetchOne.
	if _, ok := c.Resolve(context.Background(), "unknown"); ok {
		t.Fatal("unknown slug must not resolve")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("first unknown: FetchOne calls = %d, want 1", got)
	}
	// Second unknown lookup within TTL -> negative cache suppresses the lookup.
	if _, ok := c.Resolve(context.Background(), "unknown"); ok {
		t.Fatal("unknown slug must still not resolve")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("second unknown: FetchOne calls = %d, want still 1 (negative-cached)", got)
	}
}

func TestClientResolve_OnMissPositiveInsertedIntoOverlay(t *testing.T) {
	empty := mustBody(t, routesPayload{Routes: map[string]routeEntry{}})
	one := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"late": {MeshIP: "100.64.0.7", Port: 3000, Visibility: "public"},
	}})
	src := &fakeSource{
		fetchFn:    func(ctx context.Context, etag string) (int, string, []byte, error) { return 200, "e1", empty, nil },
		fetchOneFn: func(ctx context.Context, slug string) (int, []byte, error) { return 200, one, nil },
	}
	c := NewClient(ClientConfig{Source: src})
	c.pollOnce(context.Background())

	r, ok := c.Resolve(context.Background(), "late")
	if !ok || r.Port != 3000 {
		t.Fatalf("on-miss positive should resolve: ok=%v r=%+v", ok, r)
	}
	// Second lookup served from the overlay, no second FetchOne.
	if _, ok := c.Resolve(context.Background(), "late"); !ok {
		t.Fatal("overlay lookup should resolve")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("FetchOne calls = %d, want 1 (overlay caches the positive)", got)
	}
}

func TestClient_ServesLastGoodAfterFetchFailure(t *testing.T) {
	full := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"app": {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
	}})
	var calls int32
	src := &fakeSource{
		fetchFn: func(ctx context.Context, etag string) (int, string, []byte, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return 200, "e1", full, nil
			}
			return 0, "", nil, errors.New("control plane unreachable")
		},
		fetchOneFn: func(ctx context.Context, slug string) (int, []byte, error) { return 404, nil, nil },
	}
	c := NewClient(ClientConfig{Source: src})
	c.pollOnce(context.Background()) // 200
	c.pollOnce(context.Background()) // error

	if !c.FetchedOnce() {
		t.Fatal("FetchedOnce should stay true after a later failure")
	}
	if _, ok := c.Resolve(context.Background(), "app"); !ok {
		t.Fatal("must keep serving last-good routes through a fetch failure")
	}
}

func TestNegativeCache_ExpiresAndIsBounded(t *testing.T) {
	now := time.Unix(0, 0)
	n := newNegativeCache(time.Second, 3)
	n.now = func() time.Time { return now }
	n.add("a")
	if !n.has("a") {
		t.Fatal("a should be cached")
	}
	now = now.Add(2 * time.Second)
	if n.has("a") {
		t.Fatal("a should have expired")
	}
	// Bounded: adding beyond maxEntries never grows unbounded.
	now = time.Unix(100, 0)
	for i := 0; i < 50; i++ {
		n.add(fmt.Sprintf("s%d", i))
	}
	if len(n.entries) > 3 {
		t.Fatalf("negative cache grew to %d, want <= 3", len(n.entries))
	}
}

func TestDecodeRoute_UnknownVisibilityDefaultsGated(t *testing.T) {
	now := time.Unix(1, 0)
	body := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"absent": {MeshIP: "100.64.0.1", Port: 8080, Visibility: ""},
		"weird":  {MeshIP: "100.64.0.2", Port: 8080, Visibility: "somethingelse"},
		"public": {MeshIP: "100.64.0.3", Port: 8080, Visibility: "public"},
		"gated":  {MeshIP: "100.64.0.4", Port: 8080, Visibility: "GATED"}, // case-insensitive
	}})
	m, err := applyRoutesResponse(nil, 200, "e", body, now)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown/absent fails CLOSED to gated (requires authz), never to public.
	if m.routes["absent"].Visibility != VisibilityGated {
		t.Errorf("absent visibility should default gated, got %q", m.routes["absent"].Visibility)
	}
	if m.routes["weird"].Visibility != VisibilityGated {
		t.Errorf("unknown visibility should default gated, got %q", m.routes["weird"].Visibility)
	}
	if m.routes["public"].Visibility != VisibilityPublic {
		t.Errorf("explicit public should stay public, got %q", m.routes["public"].Visibility)
	}
	if m.routes["gated"].Visibility != VisibilityGated {
		t.Errorf("gated (any case) should be gated, got %q", m.routes["gated"].Visibility)
	}
}

func TestClientFresh_NilMapAndExpiryFailClosed(t *testing.T) {
	full := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"app": {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
	}})
	src := &fakeSource{
		fetchFn:    func(context.Context, string) (int, string, []byte, error) { return 200, "e1", full, nil },
		fetchOneFn: func(context.Context, string) (int, []byte, error) { return 404, nil, nil },
	}
	c := NewClient(ClientConfig{Source: src, MaxAge: time.Minute})
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	// Config-absent: never fetched -> not fresh, Resolve fails closed (no dial).
	if c.Fresh() {
		t.Fatal("a never-fetched client must not be fresh")
	}
	if _, ok := c.Resolve(context.Background(), "app"); ok {
		t.Fatal("resolve must fail closed before any successful fetch")
	}

	c.pollOnce(context.Background()) // 200 -> fetchedAt = now
	if !c.Fresh() {
		t.Fatal("fresh right after a 200 fetch")
	}
	if _, ok := c.Resolve(context.Background(), "app"); !ok {
		t.Fatal("resolve should hit within maxAge")
	}

	// Advance past maxAge: expired -> fail closed on both Fresh and Resolve.
	now = now.Add(time.Minute + time.Second)
	if c.Fresh() {
		t.Fatal("must expire past maxAge")
	}
	if _, ok := c.Resolve(context.Background(), "app"); ok {
		t.Fatal("resolve must fail closed once the map is expired")
	}
}

func TestClient_304RefreshesFreshness(t *testing.T) {
	full := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"app": {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
	}})
	var calls int32
	src := &fakeSource{
		fetchFn: func(_ context.Context, _ string) (int, string, []byte, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return 200, "e1", full, nil
			}
			return 304, "e1", nil, nil // unchanged but re-validated
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) { return 404, nil, nil },
	}
	c := NewClient(ClientConfig{Source: src, MaxAge: time.Minute})
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	c.pollOnce(context.Background()) // 200 at t=1000 -> fetchedAt=1000
	now = time.Unix(1050, 0)
	c.pollOnce(context.Background()) // 304 at t=1050 -> re-stamped fetchedAt=1050

	// t=1100: 50s since the 304 (fresh). Would have been 100s since the 200
	// (expired) had the 304 not reset the clock.
	now = time.Unix(1100, 0)
	if !c.Fresh() {
		t.Fatal("a 304 must reset the freshness clock so a healthy feed does not expire")
	}
	if _, ok := c.Resolve(context.Background(), "app"); !ok {
		t.Fatal("resolve should still hit after a 304 re-stamp")
	}
}

func TestLookup_Decisions(t *testing.T) {
	m := &RouteMap{routes: map[string]Route{"x": {Slug: "x", MeshIP: netip.MustParseAddr("100.64.0.1"), Port: 1}}}
	neg := newNegativeCache(time.Minute, 10)

	if _, d := lookup(m, "x", neg); d != decisionHit {
		t.Errorf("known: decision = %v, want hit", d)
	}
	if _, d := lookup(m, "y", neg); d != decisionMiss {
		t.Errorf("unknown-fresh: decision = %v, want miss", d)
	}
	neg.add("y")
	if _, d := lookup(m, "y", neg); d != decisionNegative {
		t.Errorf("unknown-negcached: decision = %v, want negative", d)
	}
}
