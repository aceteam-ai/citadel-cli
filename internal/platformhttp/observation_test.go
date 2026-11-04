package platformhttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestObserverUsesActualHTTPStatusAndErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int64
		body   string
		want   Outcome
	}{
		{"2xx", 200, oldBody, HTTPHealthy},
		{"wrong edge", 404, railwayBody, HTTPWrongAnswer},
		{"platform 5xx", 503, "{\"error\":\"unavailable\"}", HTTPWrongAnswer},
		{"mismatch", 400, railwayBody, HTTPWrongAnswer},
		{"validation", 400, "{\"error\":\"invalid_input\"}", HTTPOther},
		{"auth", 401, "private-refusal", HTTPOther},
		{"scope", 403, "private-refusal", HTTPOther},
		{"quota opaque", 429, "quota-refusal", HTTPOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwapFixture(t, false)
			var observations []Observation
			f.transport.observer = func(o Observation) { observations = append(observations, o) }
			f.failure.Store(tc.status)
			f.payload.Store(tc.body)
			f.request(t, "/actual")
			if len(observations) != 1 || observations[0].Outcome != tc.want || observations[0].Status != int(tc.status) || observations[0].At.IsZero() {
				t.Fatalf("actual HTTP provenance = %+v", observations)
			}
		})
	}
}

func TestObserverNetworkFailureVersusCallerCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "dns", true: "caller cancel"}[cancelled], func(t *testing.T) {
			var observations []Observation
			tr := New("http://platform.example", Options{BaseTransport: &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, &net.DNSError{Err: "synthetic missing host", Name: "platform.example", IsNotFound: true}
				},
			}, Observe: func(o Observation) { observations = append(observations, o) }, Logf: func(string, ...any) {}})
			defer tr.CloseIdleConnections()
			ctx := context.Background()
			if cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://platform.example/api", nil)
			if _, err := tr.RoundTrip(req); err == nil {
				t.Fatal("fixture unexpectedly connected")
			}
			if cancelled && len(observations) != 0 {
				t.Fatal("caller cancellation fabricated platform failure")
			}
			if !cancelled && (len(observations) != 1 || observations[0].Outcome != HTTPUnavailable || observations[0].Status != 0) {
				t.Fatal("DNS failure was not offline evidence")
			}
		})
	}
}

func TestRetiredBadResponseCannotProveFreshDialFailed(t *testing.T) {
	f := newSwapFixture(t, true)
	var h Health
	f.transport.observer = h.Observe
	f.failure.Store(404)
	resp, err := f.client.Get(f.baseURL + "/old")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	f.transport.ForceRedial()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().FailingSince != nil {
		t.Fatal("retired response armed a fresh-pool failure streak")
	}
	f.target.Store(f.next.Listener.Addr().String())
	f.request(t, "/fresh")
	if h.Snapshot().OKAt == nil || f.dials.Load() != 2 {
		t.Fatal("forced recovery did not re-dial or record actual success")
	}
	if h.NextAction(time.Unix(1700000000, 0).Add(time.Hour), time.Minute, time.Minute) != NoRecovery {
		t.Fatal("success left recovery armed")
	}
}
