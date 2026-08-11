// cmd/status_probe_test.go
//
// Regression tests for the `citadel status` Pub/Sub probe (citadel-cli#735).
//
// The bug: the probe read worker.pubsub_transport off /status, which runs a full
// collection (docker stats per running service plus nvidia-smi). On a gateway
// node that collection measured 1.98-2.67s against a 2s probe bound, so the line
// degraded to "unknown" on 9 of 10 runs on a node whose status server was
// enabled and answering 200, and the "unknown" text told the operator to
// enable a feature that was already on.
//
// These tests pin both halves: the transport is readable while /status is slow,
// and a timeout no longer reports as an absent server.
package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// pointProbeAtServer makes probeWorkerPubSubTransport() (which dials 127.0.0.1
// at the recorded status port) reach the given test server.
func pointProbeAtServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	_, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	pointProbeAtPort(t, port)
}

func pointProbeAtPort(t *testing.T, port int) {
	t.Helper()
	prev := provisionedStateDirOverride
	t.Cleanup(func() { provisionedStateDirOverride = prev })
	dir := t.TempDir()
	provisionedStateDirOverride = dir
	writeFactsFile(t, dir, gatewayFacts{Port: 8443, UseTLS: true, StatusPort: port})
}

// TestProbeWorkerPubSubTransportSurvivesSlowStatus is the #735 regression test.
//
// It models the reported node exactly: a worker too old to serve /worker (404),
// whose /status takes longer than the old 2s bound. The probe must still report
// the transport rather than "unknown".
//
// This is also the test that pins the real defect. The old code passed
// `&http.Client{Timeout: 2 * time.Second}`, but httpGetBody ALSO applied its own
// hardcoded 2s context deadline, so raising the client timeout alone changed
// nothing. Reinstating that inner constant must fail this test.
func TestProbeWorkerPubSubTransportSurvivesSlowStatus(t *testing.T) {
	var statusHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker":
			// Pre-#735 worker: no such route.
			http.NotFound(w, r)
		case "/status":
			statusHits++
			// Straddles the old 2s bound the way the measured collection did.
			time.Sleep(3 * time.Second)
			fmt.Fprint(w, `{"worker":{"consuming":true,"pubsub_transport":"websocket"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	pointProbeAtServer(t, srv)

	transport, state := probeWorkerPubSubTransport()
	if state != pubSubProbeOK || transport != "websocket" {
		t.Fatalf("probeWorkerPubSubTransport() = (%q, %v), want (\"websocket\", pubSubProbeOK): "+
			"a slow /status must not read as an unreachable status server", transport, state)
	}
	if statusHits != 1 {
		t.Errorf("/status hits = %d, want 1 (the 404 on /worker must fall back exactly once)", statusHits)
	}
}

// TestProbeWorkerPubSubTransportPrefersCheapEndpoint pins the decoupling: on a
// worker that serves /worker, the probe must not touch /status at all, so the
// answer no longer depends on how long a docker/nvidia sweep takes.
func TestProbeWorkerPubSubTransportPrefersCheapEndpoint(t *testing.T) {
	var statusHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker":
			fmt.Fprint(w, `{"worker":{"consuming":true,"pubsub_transport":"http"}}`)
		case "/status":
			statusHits++
			time.Sleep(5 * time.Second)
			fmt.Fprint(w, `{"worker":{"consuming":true,"pubsub_transport":"websocket"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	pointProbeAtServer(t, srv)

	start := time.Now()
	transport, state := probeWorkerPubSubTransport()
	elapsed := time.Since(start)

	if state != pubSubProbeOK || transport != "http" {
		t.Fatalf("probeWorkerPubSubTransport() = (%q, %v), want (\"http\", pubSubProbeOK)", transport, state)
	}
	if statusHits != 0 {
		t.Errorf("/status was collected %d time(s); the cheap endpoint must make the full sweep unnecessary", statusHits)
	}
	if elapsed > 2*time.Second {
		t.Errorf("probe took %s; it must not wait on the full collection", elapsed)
	}
}

// TestProbeWorkerPubSubTransportDoesNotFallBackOnNon404 pins the OTHER half of
// the version-skew branch: only a 404 buys the expensive fallback.
//
// 404 is the one status that means "this build has no /worker route" (the status
// server's mux registers no "/" catch-all, so net/http answers it). Any other
// status came from a server that answered, and re-asking that server for a full
// collection would pay the 10s sweep this change exists to avoid, on a node that
// is already unhealthy.
//
// It also pins the message: a server that answered must not be reported as one
// the operator still needs to enable.
func TestProbeWorkerPubSubTransportDoesNotFallBackOnNon404(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var statusHits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/worker":
					http.Error(w, "upstream unavailable", code)
				case "/status":
					statusHits++
					fmt.Fprint(w, `{"worker":{"consuming":true,"pubsub_transport":"websocket"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			pointProbeAtServer(t, srv)

			_, state := probeWorkerPubSubTransport()
			if statusHits != 0 {
				t.Errorf("/status was collected %d time(s) after a %d on /worker; only a 404 means "+
					"\"no such route\", and the full sweep is exactly the cost being avoided", statusHits, code)
			}
			if state == pubSubProbeUnreachable {
				t.Errorf("probe state = pubSubProbeUnreachable after a %d; the server ANSWERED, so telling the "+
					"operator to enable --status-port/--gateway points at a setting that is not the problem", code)
			}
			if state != pubSubProbeBadStatus {
				t.Errorf("probe state = %v, want pubSubProbeBadStatus", state)
			}
		})
	}
}

// TestProbeWorkerPubSubTransportDistinguishesTimeoutFromAbsent covers the second
// half of #735: "timed out" and "not enabled" are different operator actions and
// used to print the same string.
func TestProbeWorkerPubSubTransportDistinguishesTimeoutFromAbsent(t *testing.T) {
	t.Run("nothing listening reads as unreachable", func(t *testing.T) {
		// Bind and immediately release a port so the dial is refused rather than
		// filtered (a filtered port would hang, which is a different verdict).
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_, portStr, _ := net.SplitHostPort(ln.Addr().String())
		port, _ := strconv.Atoi(portStr)
		ln.Close()
		pointProbeAtPort(t, port)

		if _, state := probeWorkerPubSubTransport(); state != pubSubProbeUnreachable {
			t.Fatalf("probe state = %v, want pubSubProbeUnreachable", state)
		}
	})

	t.Run("listening but wedged reads as timed out", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
		// Cleanups run last-registered-first: unblock the handler before Close,
		// which waits on outstanding requests.
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		pointProbeAtServer(t, srv)

		// Shrink the bounds so the test does not sit out the production ones.
		prevCheap, prevFull := pubSubProbeCheapTimeout, pubSubProbeFullTimeout
		t.Cleanup(func() { pubSubProbeCheapTimeout, pubSubProbeFullTimeout = prevCheap, prevFull })
		pubSubProbeCheapTimeout, pubSubProbeFullTimeout = 150*time.Millisecond, 150*time.Millisecond

		if _, state := probeWorkerPubSubTransport(); state != pubSubProbeTimedOut {
			t.Fatalf("probe state = %v, want pubSubProbeTimedOut: a server that IS listening must not be "+
				"reported as one the operator still needs to enable", state)
		}
	})
}

// TestHTTPGetBodyHonorsClientTimeout pins the fix at its source: the inner
// deadline must come from the caller's client, not a constant. A caller that
// asks for longer than 2s and does not get it is the #735 defect in miniature.
func TestHTTPGetBodyHonorsClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2500 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	body, err := httpGetBodyErr(&http.Client{Timeout: 8 * time.Second}, srv.URL)
	if err != nil {
		t.Fatalf("httpGetBodyErr with an 8s client timeout failed after ~2.5s: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

// TestHTTPGetBodyErrReportsStatus pins the 404 signal the version-skew fallback
// depends on: a non-2xx must be distinguishable from a transport failure.
func TestHTTPGetBodyErrReportsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := httpGetBodyErr(&http.Client{Timeout: time.Second}, srv.URL)
	// errors.As, matching the predicate the /status fallback actually branches
	// on. A bare type assertion would pass here and still let a future wrap break
	// the fallback silently.
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %T (%v), want *httpStatusError", err, err)
	}
	if statusErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", statusErr.StatusCode)
	}
	if isTimeoutError(err) {
		t.Error("a 404 must not classify as a timeout")
	}
}
