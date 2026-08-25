// internal/jobs/extraction_handler_ready_test.go
//
// waitForReady must distinguish "this node does not run the extraction service"
// from "the service is here but not ready yet" (#653 residual).
package jobs

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pinExtractionPort points the handler's target port at one of the test's
// choosing for the duration of the test.
func pinExtractionPort(t *testing.T, port int) {
	t.Helper()
	original := extractionHostPort
	extractionHostPort = port
	t.Cleanup(func() { extractionHostPort = original })
}

// shrinkExtractionBounds compresses the readiness budgets so the behavioural
// tests run in milliseconds. The RATIO is what each test depends on (short grace
// << long budget); the absolute production values are pinned separately by
// TestExtractionReadinessDefaults.
func shrinkExtractionBounds(t *testing.T) {
	t.Helper()
	maxW, notRunning, poll, dial := extractionMaxWait, extractionNotRunningWait, extractionPollInterval, extractionDialTimeout
	extractionMaxWait = 3 * time.Second
	extractionNotRunningWait = 300 * time.Millisecond
	extractionPollInterval = 50 * time.Millisecond
	extractionDialTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		extractionMaxWait, extractionNotRunningWait, extractionPollInterval, extractionDialTimeout = maxW, notRunning, poll, dial
	})
}

// TestExtractionReadinessDefaults pins the SHIPPED bounds. The behavioural tests
// shrink them for speed, so without this a change to the real budgets -- or a
// test helper that forgot to restore them -- would go unnoticed.
func TestExtractionReadinessDefaults(t *testing.T) {
	if extractionNotRunningWait != 8*time.Second {
		t.Errorf("not-running grace = %v, want 8s", extractionNotRunningWait)
	}
	if extractionMaxWait != 60*time.Second {
		t.Errorf("startup budget = %v, want 60s", extractionMaxWait)
	}
	if extractionNotRunningWait >= extractionMaxWait {
		t.Error("the not-running grace must be well under the startup budget, or the split buys nothing")
	}
}

// freeClosedPort returns a port that is guaranteed to have nothing listening.
func freeClosedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// TestWaitForReadyNotRunningFailsFastAndSaysSo is the reported residual: with
// nothing listening, the handler used to burn the FULL 60s startup budget before
// erroring. If the platform's dispatch budget is shorter, the caller sees a bare
// timeout -- the same signature as dead hardware -- for what is really "this
// node does not provide that capability".
//
// Asserts both halves, because either alone is passable by a wrong
// implementation: the elapsed bound (it no longer waits out the long budget) AND
// the message (it says which of the two failures happened).
func TestWaitForReadyNotRunningFailsFastAndSaysSo(t *testing.T) {
	shrinkExtractionBounds(t)
	pinExtractionPort(t, freeClosedPort(t))

	h := &ExtractionHandler{}
	start := time.Now()
	err := h.waitForReady()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when nothing is listening")
	}
	if elapsed >= extractionMaxWait {
		t.Errorf("waited %v -- must give up after the short not-running grace (%v), not the full startup budget (%v)",
			elapsed, extractionNotRunningWait, extractionMaxWait)
	}
	if !strings.Contains(err.Error(), "not running on this node") {
		t.Errorf("error must identify this as the service being absent, got: %v", err)
	}
}

// TestWaitForReadyReturnsOnHealthy is the healthy path, so the test above cannot
// pass by waitForReady always failing.
func TestWaitForReadyReturnsOnHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pinExtractionPort(t, srv.Listener.Addr().(*net.TCPAddr).Port)

	h := &ExtractionHandler{}
	if err := h.waitForReady(); err != nil {
		t.Fatalf("healthy service must be reported ready, got: %v", err)
	}
}

// TestWaitForReadyListeningButUnhealthyKeepsTheLongBudget pins the case the
// fast-fail must NOT steal. A service that has bound its port but is still
// loading its model is legitimately slow, and cutting it off after the short
// grace would break a working deploy -- turning a fix for a misleading message
// into a real regression.
//
// Runs against a server that answers 503 forever; asserts the handler is still
// waiting well past the short grace rather than giving up. It does not run to
// the full 60s (that would make the suite unbearable) -- outliving the grace is
// the discriminating observation.
func TestWaitForReadyListeningButUnhealthyKeepsTheLongBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	shrinkExtractionBounds(t)
	pinExtractionPort(t, srv.Listener.Addr().(*net.TCPAddr).Port)

	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h := &ExtractionHandler{}
		done <- h.waitForReady()
	}()

	// t.Cleanup runs LIFO, and this is registered after shrinkExtractionBounds'/
	// pinExtractionPort's cleanups above, so it runs FIRST: it drains the
	// goroutine (bounded by extractionMaxWait) before those restore the
	// package-level bounds/port vars the goroutine's waitForReady loop is still
	// reading. Without this, the still-running goroutine's reads race the
	// restoring cleanups' writes to the same vars once this test function
	// returns (#810). A dedicated close-only channel (rather than draining
	// `done` again) matters on the failure branch below: the select there
	// already consumes the one buffered value off `done`, and `t.Fatalf` calls
	// runtime.Goexit() straight into this cleanup, so a second `<-done` would
	// deadlock with no sender left. Receiving on a closed channel is always
	// immediate, regardless of which branch ran.
	t.Cleanup(func() { <-finished })

	select {
	case err := <-done:
		t.Fatalf("gave up after the short grace on a service that IS listening (err=%v); "+
			"the long budget must still apply once the port answers", err)
	case <-time.After(extractionNotRunningWait * 3):
		// Still waiting, which is correct. The goroutine is left to finish on its
		// own; it is bounded by extractionMaxWait, and the Cleanup above ensures
		// it has actually finished before the bounds it depends on are restored.
	}
}
