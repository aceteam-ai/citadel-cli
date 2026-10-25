package platformhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureRoundTripper struct{}

func (*fixtureRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("synthetic transport")
}

func TestClientCopyPreservesTimeoutTLSAndRedirectCustody(t *testing.T) {
	base := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	original := &http.Client{Transport: base, Timeout: 7 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	copy := WithTransport("https://platform.example", original)
	bounded, ok := copy.Transport.(*Transport)
	if !ok || copy == original || original.Transport != base || copy.Timeout != original.Timeout || copy.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("client policy was changed instead of privately copied")
	}
	defer bounded.CloseIdleConnections()
	if bounded.template.TLSClientConfig.MinVersion != tls.VersionTLS13 || base.IdleConnTimeout != 0 {
		t.Fatal("custom TLS policy or original transport changed")
	}
	custom := &fixtureRoundTripper{}
	original.Transport = custom
	if copy := WithTransport("https://platform.example", original); copy.Transport != custom {
		t.Fatal("explicit non-standard transport custody was replaced")
	}
}

func TestOpaqueDefaultTransportIsNotReplacedWithNetworkAccess(t *testing.T) {
	original := http.DefaultTransport
	custom := &fixtureRoundTripper{}
	http.DefaultTransport = custom
	defer func() { http.DefaultTransport = original }()
	tr := New("https://platform.example", Options{})
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "https://platform.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(req); err == nil || err.Error() != "synthetic transport" {
		t.Fatal("explicit opaque default transport was bypassed")
	}
}

func TestFreshDialDiagnosticsAreDebugOnly(t *testing.T) {
	f := newSwapFixture(t, false)
	f.request(t, "/normal")
	if f.diagnostic() != "" {
		t.Fatal("ordinary fresh dial printed over normal/TUI output")
	}
	var debug []string
	f.transport.debugf = func(format string, args ...any) {
		debug = append(debug, fmt.Sprintf(format, args...))
	}
	f.client.CloseIdleConnections()
	f.request(t, "/debug")
	if len(debug) != 1 || !strings.Contains(debug[0], "fresh dial remote_ip=127.0.0.1 reason=initial") || f.diagnostic() != "" {
		t.Fatal("fresh-dial metadata did not stay on the opt-in debug path")
	}
}

func TestCancellationAndSecondaryOriginDoNotContributeFailures(t *testing.T) {
	f := newSwapFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range FailureThreshold {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/cancelled", nil)
		if _, err := f.transport.RoundTrip(req); err == nil {
			t.Fatal("cancelled request unexpectedly succeeded")
		}
	}
	for range FailureThreshold {
		req, _ := http.NewRequest(http.MethodGet, "http://secondary.example/failure", nil)
		if _, err := f.transport.RoundTrip(req); err == nil {
			t.Fatal("fixture accepted a non-platform host")
		}
	}
	if f.transport.current.failed != 0 || f.dials.Load() != 0 {
		t.Fatal("cancellation/other origin altered the platform failure streak")
	}
}

func TestAuthorizationBodiesAreNotObserved(t *testing.T) {
	for _, status := range []int64{401, 403} {
		f := newSwapFixture(t, false)
		f.failure.Store(status)
		f.payload.Store("synthetic-refusal-secret")
		resp, err := f.client.Get(f.baseURL + "/auth")
		if err != nil {
			t.Fatal(err)
		}
		if _, wrapped := resp.Body.(*observedBody); wrapped {
			t.Fatal("authorization refusal body was retained for observation")
		}
		body, err := io.ReadAll(resp.Body) // Only the caller chooses to read.
		resp.Body.Close()
		if err != nil || string(body) != "synthetic-refusal-secret" || strings.Contains(f.diagnostic(), "synthetic-refusal-secret") {
			t.Fatal("authorization body or diagnostic privacy changed")
		}
	}
}

func TestErrorSampleIsBoundedAndBodyUnchanged(t *testing.T) {
	source := strings.Repeat("x", maxErrorSample*3)
	var calls atomic.Int32
	body := &observedBody{ReadCloser: io.NopCloser(strings.NewReader(source)), finish: func(sample []byte, complete bool) {
		calls.Add(1)
		if len(sample) != maxErrorSample+1 || complete {
			t.Error("error-body observation was not bounded")
		}
	}}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(got) != source || calls.Load() != 1 || body.sample != nil {
		t.Fatal("observation changed bytes or retained a completed sample")
	}
}

type closingReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *closingReader) Read(p []byte) (int, error) {
	close(r.entered)
	<-r.release
	return copy(p, "fixture"), io.EOF
}

func (r *closingReader) Close() error {
	r.once.Do(func() { close(r.release) })
	return nil
}

func TestBodyCloseCanUnblockAConcurrentRead(t *testing.T) {
	reader := &closingReader{entered: make(chan struct{}), release: make(chan struct{})}
	var callbacks atomic.Int32
	body := &observedBody{ReadCloser: reader, finish: func([]byte, bool) { callbacks.Add(1) }}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = body.Read(make([]byte, 32))
	}()
	<-reader.entered
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	<-finished
	if callbacks.Load() != 1 {
		t.Fatal("concurrent close/read completed the observation more than once")
	}
}

func TestBinarySuccessAndRateLimitHeadersRemainUnchanged(t *testing.T) {
	want := []byte{0, 255, 1, 2, 3}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(want)
	}))
	defer server.Close()
	client := &http.Client{Transport: New(server.URL, Options{Logf: func(string, ...any) {}}), Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := resp.Body.(*observedBody); wrapped {
		t.Fatal("binary success was wrapped as an error observation")
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("binary success bytes changed")
	}
	f := newSwapFixture(t, false)
	f.failure.Store(429)
	f.payload.Store("{\"error\":\"rate_limited\",\"retry_after\":60}")
	resp, err = f.client.Get(f.baseURL + "/limited")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "60" || f.oldCalls.Load() != 1 {
		t.Fatal("rate-limit status, hint or retry budget changed")
	}
}
