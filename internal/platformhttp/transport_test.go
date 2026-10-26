package platformhttp

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStableHostnameRecoversAfterFailuresAcrossEndpoints(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			f := newSwapFixture(t, h2)
			f.request(t, "/warm")
			f.target.Store(f.next.Listener.Addr().String()) // DNS moves; old edge stays alive.
			f.failure.Store(404)
			for _, path := range []string{"/status?secret=query-canary", "/desired-state", "/pubsub"} {
				if status, body := f.request(t, path); status != 404 || body != railwayBody {
					t.Fatal("transport changed/replayed the old edge response")
				}
				if f.dials.Load() != 1 {
					t.Fatal("fixture did not retain the old connection until the threshold")
				}
			}
			if status, body := f.request(t, "/after"); status != 200 || body != newBody {
				t.Fatal("next attempt did not reach replacement server")
			}
			if f.dials.Load() != 2 || f.oldCalls.Load() != 4 || f.newCalls.Load() != 1 {
				t.Fatal("expected exactly one fresh dial and no request replay")
			}
			logs := f.diagnostic()
			if strings.Count(logs, "re-dialing") != 1 || !strings.Contains(logs, "upstream mismatch: re-dialing remote_ip=127.0.0.1") {
				t.Fatal("missing safe mismatch/IP diagnostics")
			}
			for _, secret := range []string{"query-canary", "synthetic-credential", "body-canary", "Application not found", "platform.example"} {
				if strings.Contains(logs, secret) {
					t.Fatal("transport diagnostics exposed request/response content")
				}
			}
		})
	}
}

func TestMaximumAgeRedialsHealthyAndBusyConnections(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			f := newSwapFixture(t, h2)
			f.request(t, "/warm")
			f.request(t, "/still-old")
			held, err := f.client.Get(f.baseURL + "/held")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { held.Body.Close() })
			f.target.Store(f.next.Listener.Addr().String())
			f.clock.Add(int64(MaxConnectionAge))
			if status, body := f.request(t, "/after-age"); status != 200 || body != newBody || f.dials.Load() != 2 {
				t.Fatal("maximum age failed to bypass the still-healthy old pool")
			}
			if !strings.Contains(f.diagnostic(), "max_connection_age: re-dialing remote_ip=127.0.0.1") {
				t.Fatal("missing maximum-age/IP diagnostic")
			}
			if f.heldCancel.Load() {
				t.Fatal("pool rotation cancelled an in-flight request")
			}
			f.once.Do(func() { close(f.release) })
			body, err := io.ReadAll(held.Body)
			if err != nil || string(body) != "held-finished" {
				t.Fatalf("in-flight response did not finish unchanged: %v", err)
			}
		})
	}
}

func TestFailureClassesAndPlatformErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int64
		redial     bool
	}{
		{"platform 5xx", "{\"error\":\"temporarily_unavailable\"}", 503, true},
		{"platform 404 across paths", "{\"error\":\"not_found\"}", 404, true},
		{"non-platform 400", railwayBody, 400, true},
		{"large opaque error", strings.Repeat("<html>", maxErrorSample), 400, true},
		{"platform validation", "{\"error\":\"invalid_request\"}", 400, false},
		{"platform rate limit", "{\"error\":\"rate_limited\",\"retry_after\":60}", 429, false},
		{"authorization", "{\"error\":\"synthetic-private-refusal\"}", 401, false},
		{"scope refusal", "{\"error\":\"synthetic-private-refusal\"}", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwapFixture(t, false)
			f.request(t, "/warm")
			f.target.Store(f.next.Listener.Addr().String())
			f.failure.Store(tc.status)
			f.payload.Store(tc.body)
			for _, path := range []string{"/one", "/two", "/three"} {
				if status, body := f.request(t, path); int64(status) != tc.status || body != tc.body {
					t.Fatal("error body or status changed")
				}
			}
			status, _ := f.request(t, "/after")
			if tc.redial && (status != 200 || f.dials.Load() != 2) {
				t.Fatal("failure window did not force a fresh dial")
			}
			if !tc.redial && (int64(status) != tc.status || f.dials.Load() != 1) {
				t.Fatal("canonical refusal changed connection/retry behavior")
			}
		})
	}
}

func TestSuccessfulResponseResetsConsecutiveFailures(t *testing.T) {
	f := newSwapFixture(t, false)
	f.request(t, "/warm")
	f.target.Store(f.next.Listener.Addr().String())
	f.failure.Store(404)
	for _, path := range []string{"/fail-one", "/fail-two"} {
		f.request(t, path)
	}
	f.failure.Store(0)
	f.request(t, "/healthy")
	f.failure.Store(404)
	for _, path := range []string{"/again-one", "/again-two"} {
		f.request(t, path)
	}
	if f.dials.Load() != 1 {
		t.Fatal("successful response failed to reset the streak")
	}
	f.request(t, "/third")
	if status, _ := f.request(t, "/after"); status != 200 || f.dials.Load() != 2 {
		t.Fatal("fresh consecutive streak did not rotate")
	}
}

func TestDefaultsAndRetiredResponsesCannotRotateNewPool(t *testing.T) {
	base := &http.Transport{IdleConnTimeout: 2 * time.Minute}
	tr := New("https://platform.example", Options{BaseTransport: base, Logf: func(string, ...any) {}})
	defer tr.CloseIdleConnections()
	if tr.current.pool.IdleConnTimeout != IdleConnTimeout || base.IdleConnTimeout != 2*time.Minute || tr.maxAge != MaxConnectionAge {
		t.Fatal("defaults were not applied to a private transport")
	}
	old := tr.current
	for i := range FailureThreshold {
		tr.observe(old, true, "fixture_failure", "127.0.0.1", fmt.Sprintf("/fixture/%d", i))
	}
	current := tr.current
	tr.observe(current, true, "fixture_failure", "127.0.0.1", "/current")
	tr.observe(old, false, "", "127.0.0.1", "/old")
	tr.observe(old, true, "fixture_failure", "127.0.0.1", "/old")
	if tr.current != current || current.failed != 1 {
		t.Fatal("late retired responses changed the replacement state")
	}
}

func TestOneFailingPathCannotCauseRotationStorm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int64
		body   string
	}{
		{"platform 404", 404, "{\"error\":\"not_found\"}"},
		{"mismatched 404", 404, railwayBody},
		{"platform 503", 503, "{\"error\":\"unavailable\"}"},
		{"mismatched 400", 400, railwayBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwapFixture(t, false)
			f.failure.Store(tc.status)
			f.payload.Store(tc.body)
			for i := range 30 {
				f.request(t, fmt.Sprintf("/one?attempt=%d", i))
			}
			f.clock.Add(int64(2 * FailureRotationCooldown))
			f.request(t, "/one?after=window")
			if f.dials.Load() != 1 || strings.Contains(f.diagnostic(), "re-dialing") {
				t.Fatal("one failing endpoint (including query variants) rotated the pool")
			}
		})
	}
}

func TestFailureRotationCooldownAndStableHostnameRecovery(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			f := newSwapFixture(t, h2)
			f.failure.Store(503)
			// First qualifying streak recovers with no minimum initial age.
			for _, path := range []string{"/one", "/two", "/one"} {
				f.request(t, path)
			}
			for i := range 30 {
				f.request(t, fmt.Sprintf("/endpoint/%d", i%2))
			}
			if f.dials.Load() != 2 || strings.Count(f.diagnostic(), "re-dialing") != 1 {
				t.Fatal("ongoing multi-endpoint outage bypassed the cooldown")
			}
			f.clock.Add(-int64(time.Second))
			f.request(t, "/one")
			if strings.Count(f.diagnostic(), "re-dialing") != 1 {
				t.Fatal("backwards clock bypassed the cooldown")
			}
			f.clock.Add(int64(time.Second))
			f.target.Store(f.next.Listener.Addr().String())
			f.clock.Add(int64(FailureRotationCooldown - time.Nanosecond))
			f.request(t, "/one")
			if f.dials.Load() != 2 || strings.Count(f.diagnostic(), "re-dialing") != 1 {
				t.Fatal("rotation happened before the cooldown boundary")
			}
			f.clock.Add(int64(time.Nanosecond))
			f.request(t, "/two") // Retire old pool, do not replay this request.
			if status, body := f.request(t, "/recovered"); status != 200 || body != newBody || f.dials.Load() != 3 {
				t.Fatal("stable hostname did not recover when cooldown expired")
			}
			if strings.Count(f.diagnostic(), "re-dialing") != 2 {
				t.Fatal("expected one recovery log for each cooldown window")
			}
		})
	}
}

func TestMaximumAgeStillRotatesDuringFailureCooldown(t *testing.T) {
	f := newSwapFixture(t, false)
	f.transport.maxAge = time.Second
	f.failure.Store(503)
	for _, path := range []string{"/one", "/two", "/one", "/two"} {
		f.request(t, path)
	}
	if f.dials.Load() != 2 {
		t.Fatal("initial failure rotation did not establish the fixture")
	}
	f.target.Store(f.next.Listener.Addr().String())
	f.clock.Add(int64(time.Second))
	if status, _ := f.request(t, "/after-age"); status != 200 || f.dials.Load() != 3 {
		t.Fatal("failure cooldown suppressed maximum-age recovery")
	}
	if !strings.Contains(f.diagnostic(), "max_connection_age: re-dialing") {
		t.Fatal("maximum-age recovery was not independently diagnosed")
	}
}

func TestSuccessfulResponseClearsDistinctEndpointHistory(t *testing.T) {
	f := newSwapFixture(t, false)
	f.failure.Store(404)
	f.request(t, "/old-path")
	f.failure.Store(0)
	f.request(t, "/healthy")
	f.failure.Store(404)
	for range 10 {
		f.request(t, "/new-path")
	}
	if f.dials.Load() != 1 || strings.Contains(f.diagnostic(), "re-dialing") {
		t.Fatal("a successful response left old distinct-path evidence behind")
	}
}
