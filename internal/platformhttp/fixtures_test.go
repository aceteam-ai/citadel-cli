package platformhttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const railwayBody = "{\"status\":\"error\",\"code\":404,\"message\":\"Application not found\",\"canary\":\"body-canary\"}"
const oldBody = "{\"old\":true}"
const newBody = "{\"new\":true}"

type swapFixture struct {
	client     *http.Client
	transport  *Transport
	baseURL    string
	old, next  *httptest.Server
	target     atomic.Value
	payload    atomic.Value
	failure    atomic.Int64
	clock      atomic.Int64
	dials      atomic.Int32
	oldCalls   atomic.Int32
	newCalls   atomic.Int32
	heldCancel atomic.Bool
	release    chan struct{}
	once       sync.Once
	logMu      sync.Mutex
	logs       []string
	h2         bool
}

func newSwapFixture(t *testing.T, h2 bool) *swapFixture {
	t.Helper()
	f := &swapFixture{release: make(chan struct{}), h2: h2, baseURL: "http://platform.example"}
	f.payload.Store(railwayBody)
	start := func(handler http.HandlerFunc) *httptest.Server {
		s := httptest.NewUnstartedServer(handler)
		s.EnableHTTP2 = h2
		if h2 {
			s.StartTLS()
		} else {
			s.Start()
		}
		return s
	}
	f.old = start(func(w http.ResponseWriter, r *http.Request) {
		f.oldCalls.Add(1)
		if r.URL.Path == "/held" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "held-")
			w.(http.Flusher).Flush()
			select {
			case <-f.release:
				_, _ = io.WriteString(w, "finished")
			case <-r.Context().Done():
				f.heldCancel.Store(true)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status := int(f.failure.Load()); status != 0 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, f.payload.Load().(string))
			return
		}
		_, _ = io.WriteString(w, oldBody)
	})
	f.next = start(func(w http.ResponseWriter, r *http.Request) {
		f.newCalls.Add(1)
		if r.Host != "platform.example" || r.Header.Get("Authorization") != "Bearer synthetic-credential" {
			t.Error("fresh dial changed host or authorization")
		}
		_, _ = io.WriteString(w, newBody)
	})
	f.target.Store(f.old.Listener.Addr().String())
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.MaxConnsPerHost = 1
	base.ForceAttemptHTTP2 = true
	if h2 {
		f.baseURL = "https://platform.example"
		roots := x509.NewCertPool()
		roots.AddCert(f.old.Certificate())
		roots.AddCert(f.next.Certificate())
		if len(f.old.Certificate().DNSNames) == 0 {
			t.Fatal("TLS fixture needs a certificate DNS name")
		}
		base.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: f.old.Certificate().DNSNames[0]}
	}
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !strings.HasPrefix(addr, "platform.example:") {
			return nil, fmt.Errorf("fixture refused a non-platform dial")
		}
		f.dials.Add(1)
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", f.target.Load().(string))
	}
	f.transport = New(f.baseURL, Options{BaseTransport: base, Now: func() time.Time {
		return time.Unix(1700000000, f.clock.Load())
	}, Logf: func(format string, args ...any) {
		f.logMu.Lock()
		f.logs = append(f.logs, fmt.Sprintf(format, args...))
		f.logMu.Unlock()
	}})
	f.client = &http.Client{Transport: f.transport, Timeout: 10 * time.Second}
	t.Cleanup(func() {
		f.once.Do(func() { close(f.release) })
		f.client.CloseIdleConnections()
		f.old.Close()
		f.next.Close()
	})
	return f
}

func (f *swapFixture) request(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer synthetic-credential")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wantProtocol := 1
	if f.h2 {
		wantProtocol = 2
	}
	if resp.ProtoMajor != wantProtocol {
		t.Fatalf("regression fixture used HTTP/%d, want HTTP/%d", resp.ProtoMajor, wantProtocol)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func (f *swapFixture) diagnostic() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return strings.Join(f.logs, "\n")
}
