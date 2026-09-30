package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
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

	if _, ok, err := c.Resolve(context.Background(), "known"); err != nil || !ok {
		t.Fatal("known slug should resolve from the map")
	}
	if atomic.LoadInt32(&src.fetchOneCalls) != 0 {
		t.Fatal("a map hit must not trigger an on-miss lookup")
	}

	// First unknown lookup -> one on-miss FetchOne.
	if _, ok, err := c.Resolve(context.Background(), "unknown"); err != nil || ok {
		t.Fatal("unknown slug must not resolve")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("first unknown: FetchOne calls = %d, want 1", got)
	}
	// Second unknown lookup within TTL -> negative cache suppresses the lookup.
	if _, ok, err := c.Resolve(context.Background(), "unknown"); err != nil || ok {
		t.Fatal("unknown slug must still not resolve")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("second unknown: FetchOne calls = %d, want still 1 (negative-cached)", got)
	}
}

func TestClientResolve_OnMissRedirectFailsRetryablyWithoutNegativeCache(t *testing.T) {
	empty := mustBody(t, routesPayload{Routes: map[string]routeEntry{}})
	src := &fakeSource{
		fetchFn: func(context.Context, string) (int, string, []byte, error) {
			return 200, "e1", empty, nil
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) {
			return 307, nil, nil
		},
	}
	c := NewClient(ClientConfig{Source: src})
	c.pollOnce(context.Background())

	for i := 0; i < 2; i++ {
		if _, ok, err := c.Resolve(context.Background(), "redirected"); err == nil || ok {
			t.Fatalf("attempt %d: ok=%v err=%v, want retryable failure", i+1, ok, err)
		}
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 2 {
		t.Fatalf("FetchOne calls = %d, want 2 (redirect failures are not negative-cached)", got)
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

	r, ok, err := c.Resolve(context.Background(), "late")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || r.Port != 3000 {
		t.Fatalf("on-miss positive should resolve: ok=%v r=%+v", ok, r)
	}
	// Second lookup served from the overlay, no second FetchOne.
	if _, ok, err := c.Resolve(context.Background(), "late"); err != nil || !ok {
		t.Fatal("overlay lookup should resolve")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("FetchOne calls = %d, want 1 (overlay caches the positive)", got)
	}
}

func TestClientResolve_ConcurrentSameSlugCollapsesFetchOne(t *testing.T) {
	empty := mustBody(t, routesPayload{Routes: map[string]routeEntry{}})
	one := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"late": {MeshIP: "100.64.0.7", Port: 3000, Visibility: "public"},
	}})
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	src := &fakeSource{
		fetchFn: func(context.Context, string) (int, string, []byte, error) {
			return 200, "e1", empty, nil
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) {
			startedOnce.Do(func() { close(started) })
			<-release
			return 200, one, nil
		},
	}
	c := NewClient(ClientConfig{Source: src})
	c.pollOnce(context.Background())

	const callers = 32
	results := make(chan bool, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, _ := c.Resolve(context.Background(), "late")
			results <- ok
		}()
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchOne did not start")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("concurrent same-slug FetchOne calls = %d, want 1", got)
	}
	close(release)
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("collapsed caller did not receive the positive route")
		}
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("final FetchOne calls = %d, want 1", got)
	}
}

func TestClientResolve_GlobalOnMissRateCapDeniesRandomSlugScan(t *testing.T) {
	empty := mustBody(t, routesPayload{Routes: map[string]routeEntry{}})
	src := &fakeSource{
		fetchFn: func(context.Context, string) (int, string, []byte, error) {
			return 200, "e1", empty, nil
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) {
			return 404, nil, nil
		},
	}
	c := NewClient(ClientConfig{
		Source:          src,
		OnMissRateLimit: rate.Limit(1),
		OnMissBurst:     3,
	})
	now := time.Unix(1_000, 0)
	c.now = func() time.Time { return now }
	c.pollOnce(context.Background())

	type result struct {
		ok  bool
		err error
	}
	results := make(chan result, 100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := c.Resolve(context.Background(), fmt.Sprintf("scan-%d", i))
			results <- result{ok: ok, err: err}
		}()
	}
	wg.Wait()
	close(results)
	var denied int
	for result := range results {
		if result.ok {
			t.Fatal("random unknown slug unexpectedly resolved")
		}
		if result.err != nil {
			denied++
		}
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 3 {
		t.Fatalf("random-slug FetchOne calls = %d, want burst cap 3", got)
	}
	if denied != 97 {
		t.Fatalf("rate-limited results = %d, want 97", denied)
	}
}

func TestClientResolve_CacheHitsDoNotSpendOnMissBudget(t *testing.T) {
	full := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"known": {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
	}})
	src := &fakeSource{
		fetchFn: func(context.Context, string) (int, string, []byte, error) {
			return 200, "e1", full, nil
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) {
			return 404, nil, nil
		},
	}
	c := NewClient(ClientConfig{Source: src, OnMissRateLimit: rate.Limit(1), OnMissBurst: 1})
	now := time.Unix(1_000, 0)
	c.now = func() time.Time { return now }
	c.pollOnce(context.Background())

	for i := 0; i < 10; i++ {
		if _, ok, err := c.Resolve(context.Background(), "known"); err != nil || !ok {
			t.Fatalf("known lookup %d: ok=%v err=%v", i+1, ok, err)
		}
	}
	if _, ok, err := c.Resolve(context.Background(), "unknown"); err != nil || ok {
		t.Fatalf("first miss after cache hits: ok=%v err=%v", ok, err)
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 1 {
		t.Fatalf("FetchOne calls = %d, want 1", got)
	}
}

func TestClientResolve_BlockedFetchOneCannotOutliveNewerFullMap(t *testing.T) {
	empty := mustBody(t, routesPayload{Routes: map[string]routeEntry{}})
	authoritative := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"late": {MeshIP: "100.64.0.8", Port: 4000, Visibility: "gated"},
	}})
	staleOne := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"late": {MeshIP: "100.64.0.7", Port: 3000, Visibility: "public"},
	}})
	var fullCalls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	src := &fakeSource{
		fetchFn: func(context.Context, string) (int, string, []byte, error) {
			if fullCalls.Add(1) == 1 {
				return 200, "e1", empty, nil
			}
			return 200, "e2", authoritative, nil
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) {
			close(started)
			<-release
			return 200, staleOne, nil
		},
	}
	c := NewClient(ClientConfig{Source: src})
	c.pollOnce(context.Background())

	result := make(chan Route, 1)
	go func() {
		route, _, _ := c.Resolve(context.Background(), "late")
		result <- route
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchOne did not start")
	}
	c.pollOnce(context.Background())
	close(release)
	got := <-result
	if got.Port != 4000 || got.Visibility != VisibilityGated {
		t.Fatalf("blocked FetchOne returned stale route %+v, want newer authoritative route", got)
	}
	c.overlayMu.RLock()
	_, reinserted := c.overlay["late"]
	c.overlayMu.RUnlock()
	if reinserted {
		t.Fatal("blocked FetchOne reinserted an overlay after a newer 200 map")
	}
}

func TestClientResolve_BlockedNonPositiveFetchCannotOutliveNewerFullMap(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
	}{
		{name: "not found", status: 404},
		{name: "redirect", status: 307},
		{name: "transport error", err: errors.New("connection reset")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			empty := mustBody(t, routesPayload{Routes: map[string]routeEntry{}})
			authoritative := mustBody(t, routesPayload{Routes: map[string]routeEntry{
				"late": {MeshIP: "100.64.0.8", Port: 4000, Visibility: "gated"},
			}})
			var fullCalls atomic.Int32
			started := make(chan struct{})
			release := make(chan struct{})
			src := &fakeSource{
				fetchFn: func(context.Context, string) (int, string, []byte, error) {
					if fullCalls.Add(1) == 1 {
						return 200, "e1", empty, nil
					}
					return 200, "e2", authoritative, nil
				},
				fetchOneFn: func(context.Context, string) (int, []byte, error) {
					close(started)
					<-release
					return tt.status, nil, tt.err
				},
			}
			c := NewClient(ClientConfig{Source: src})
			c.pollOnce(context.Background())

			type resolution struct {
				route Route
				ok    bool
				err   error
			}
			result := make(chan resolution, 1)
			go func() {
				route, ok, err := c.Resolve(context.Background(), "late")
				result <- resolution{route: route, ok: ok, err: err}
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("FetchOne did not start")
			}
			c.pollOnce(context.Background())
			close(release)

			got := <-result
			if got.err != nil || !got.ok || got.route.Port != 4000 || got.route.Visibility != VisibilityGated {
				t.Fatalf("blocked FetchOne result = %+v, want newer authoritative route", got)
			}
			if c.neg.has("late") {
				t.Fatal("superseded FetchOne inserted a negative-cache entry")
			}
		})
	}
}

func TestClient_RequestStartControlsMapAnd304Freshness(t *testing.T) {
	full := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"app": {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
	}})
	now := time.Unix(1_000, 0)
	var calls int
	src := &fakeSource{
		fetchFn: func(context.Context, string) (int, string, []byte, error) {
			calls++
			switch calls {
			case 1:
				now = now.Add(2 * time.Minute)
				return 200, "e1", full, nil
			default:
				now = now.Add(2 * time.Minute)
				return 304, "e1", nil, nil
			}
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) { return 404, nil, nil },
	}
	c := NewClient(ClientConfig{Source: src, MaxAge: time.Minute})
	c.now = func() time.Time { return now }

	c.pollOnce(context.Background())
	if c.Fresh() {
		t.Fatal("slow 200 was stamped at response arrival instead of request send")
	}
	firstStamp := c.Current().fetchedAt
	if !firstStamp.Equal(time.Unix(1_000, 0)) {
		t.Fatalf("slow 200 fetchedAt = %v, want request start", firstStamp)
	}

	c.pollOnce(context.Background())
	if c.Fresh() {
		t.Fatal("slow 304 was stamped at response arrival instead of request send")
	}
	if want := time.Unix(1_000, 0).Add(2 * time.Minute); !c.Current().fetchedAt.Equal(want) {
		t.Fatalf("slow 304 fetchedAt = %v, want request start %v", c.Current().fetchedAt, want)
	}
}

func TestClient_RequestStartControlsOverlayAndNegativeFreshness(t *testing.T) {
	empty := mustBody(t, routesPayload{Routes: map[string]routeEntry{}})
	one := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"late": {MeshIP: "100.64.0.7", Port: 3000, Visibility: "public"},
	}})
	now := time.Unix(2_000, 0)
	var mode atomic.Int32
	src := &fakeSource{
		fetchFn: func(context.Context, string) (int, string, []byte, error) {
			return 200, "e1", empty, nil
		},
		fetchOneFn: func(context.Context, string) (int, []byte, error) {
			now = now.Add(2 * time.Second)
			if mode.Load() == 0 {
				return 200, one, nil
			}
			return 404, nil, nil
		},
	}
	c := NewClient(ClientConfig{Source: src, MaxAge: time.Second, NegativeTTL: time.Second, OnMissBurst: 10})
	c.now = func() time.Time { return now }
	c.pollOnce(context.Background())

	if _, ok, err := c.Resolve(context.Background(), "late"); ok || err == nil {
		t.Fatalf("slow FetchOne result = ok %v err %v, want freshness error", ok, err)
	}
	c.overlayMu.RLock()
	_, inserted := c.overlay["late"]
	c.overlayMu.RUnlock()
	if inserted {
		t.Fatal("slow FetchOne result exceeded maxAge but entered the overlay")
	}

	// Refresh the authoritative empty map, then prove a slow 404's negative TTL
	// starts at request send: it is already expired when the response arrives.
	c.pollOnce(context.Background())
	mode.Store(1)
	if _, ok, err := c.Resolve(context.Background(), "missing"); err != nil || ok {
		t.Fatal("missing slug unexpectedly resolved")
	}
	c.pollOnce(context.Background())
	if _, ok, err := c.Resolve(context.Background(), "missing"); err != nil || ok {
		t.Fatal("missing slug unexpectedly resolved on retry")
	}
	if got := atomic.LoadInt32(&src.fetchOneCalls); got != 3 {
		t.Fatalf("FetchOne calls = %d, want 3 (slow positive plus two expired negatives)", got)
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
	if _, ok, err := c.Resolve(context.Background(), "app"); err != nil || !ok {
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
	if _, ok, err := c.Resolve(context.Background(), "app"); err != nil || ok {
		t.Fatal("resolve must fail closed before any successful fetch")
	}

	c.pollOnce(context.Background()) // 200 -> fetchedAt = now
	if !c.Fresh() {
		t.Fatal("fresh right after a 200 fetch")
	}
	if _, ok, err := c.Resolve(context.Background(), "app"); err != nil || !ok {
		t.Fatal("resolve should hit within maxAge")
	}

	// Advance past maxAge: expired -> fail closed on both Fresh and Resolve.
	now = now.Add(time.Minute + time.Second)
	if c.Fresh() {
		t.Fatal("must expire past maxAge")
	}
	if _, ok, err := c.Resolve(context.Background(), "app"); err != nil || ok {
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
	if _, ok, err := c.Resolve(context.Background(), "app"); err != nil || !ok {
		t.Fatal("resolve should still hit after a 304 re-stamp")
	}
}

// TestClient_OverlayEntryExpiresIndependentlyUnderPerpetual304 pins FIX 3: an
// on-miss overlay entry must expire on its own maxAge even when the whole-map
// poll never advances past 304 (so the full map stays fresh forever). After
// expiry a tombstoned slug must stop resolving, re-validated by one FetchOne.
func TestClient_OverlayEntryExpiresIndependentlyUnderPerpetual304(t *testing.T) {
	full := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"known": {MeshIP: "100.64.0.1", Port: 8080, Visibility: "public"},
	}})
	lateBody := mustBody(t, routesPayload{Routes: map[string]routeEntry{
		"late": {MeshIP: "100.64.0.7", Port: 3000, Visibility: "public"},
	}})
	var fetchN, oneN int32
	var tombstoned atomic.Bool
	src := &fakeSource{
		fetchFn: func(_ context.Context, _ string) (int, string, []byte, error) {
			if atomic.AddInt32(&fetchN, 1) == 1 {
				return 200, "e1", full, nil
			}
			return 304, "e1", nil, nil // whole map stays fresh (re-stamped) forever
		},
		fetchOneFn: func(_ context.Context, _ string) (int, []byte, error) {
			atomic.AddInt32(&oneN, 1)
			if tombstoned.Load() {
				return 404, nil, nil
			}
			return 200, lateBody, nil
		},
	}
	c := NewClient(ClientConfig{Source: src, MaxAge: time.Minute})
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	c.pollOnce(context.Background()) // 200 at t=1000

	// On-miss positive: "late" resolves and enters the overlay (fetchedAt=1000).
	if _, ok, err := c.Resolve(context.Background(), "late"); err != nil || !ok {
		t.Fatal("late should resolve via on-miss FetchOne")
	}
	if got := atomic.LoadInt32(&oneN); got != 1 {
		t.Fatalf("FetchOne calls = %d, want 1", got)
	}

	// A 304 poll keeps the WHOLE map fresh at t=1030, but must not refresh the
	// overlay entry's own 1000 stamp.
	now = time.Unix(1030, 0)
	c.pollOnce(context.Background())
	if !c.Fresh() {
		t.Fatal("whole map should be fresh after a 304")
	}

	// Tombstone "late" and advance to 61s past the overlay INSERT (t=1061), while
	// the whole map is still fresh (last re-stamped at 1030). The overlay entry
	// must expire and re-validate to a 404 -> no longer resolves.
	tombstoned.Store(true)
	now = time.Unix(1061, 0)
	if _, ok, err := c.Resolve(context.Background(), "late"); err != nil || ok {
		t.Fatal("an expired overlay entry must not resolve past RouteMaxAge under a perpetual-304 feed")
	}
	if got := atomic.LoadInt32(&oneN); got != 2 {
		t.Fatalf("expired overlay must trigger one re-validating FetchOne; calls = %d, want 2", got)
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
