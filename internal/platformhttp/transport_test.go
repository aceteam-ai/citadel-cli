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
			if !strings.Contains(logs, "upstream mismatch: re-dialing remote_ip=127.0.0.1") {
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

func TestMaximumAgeRedialsHealthyAndBusyHTTP2Connections(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			f := newSwapFixture(t, h2)
			f.request(t, "/warm")
			f.request(t, "/still-old")
			var held *http.Response
			if h2 {
				var err error
				held, err = f.client.Get(f.baseURL + "/held")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { held.Body.Close() })
			}
			f.target.Store(f.next.Listener.Addr().String())
			f.clock.Add(int64(MaxConnectionAge))
			if status, body := f.request(t, "/after-age"); status != 200 || body != newBody || f.dials.Load() != 2 {
				t.Fatal("maximum age failed to bypass the still-healthy old pool")
			}
			if !strings.Contains(f.diagnostic(), "max_connection_age: re-dialing remote_ip=127.0.0.1") {
				t.Fatal("missing maximum-age/IP diagnostic")
			}
			if held != nil {
				if f.heldCancel.Load() {
					t.Fatal("pool rotation cancelled an in-flight HTTP/2 request")
				}
				f.once.Do(func() { close(f.release) })
				body, err := io.ReadAll(held.Body)
				if err != nil || string(body) != "held-finished" {
					t.Fatalf("in-flight response did not finish unchanged: %v", err)
				}
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
			for range FailureThreshold {
				if status, body := f.request(t, "/failure"); int64(status) != tc.status || body != tc.body {
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
	for range 2 {
		f.request(t, "/fail")
	}
	f.failure.Store(0)
	f.request(t, "/healthy")
	f.failure.Store(404)
	for range 2 {
		f.request(t, "/fail-again")
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
	for range FailureThreshold {
		tr.observe(old, true, "fixture_failure", "127.0.0.1")
	}
	current := tr.current
	tr.observe(current, true, "fixture_failure", "127.0.0.1")
	tr.observe(old, false, "", "127.0.0.1")
	tr.observe(old, true, "fixture_failure", "127.0.0.1")
	if tr.current != current || current.failed != 1 {
		t.Fatal("late retired responses changed the replacement state")
	}
}
