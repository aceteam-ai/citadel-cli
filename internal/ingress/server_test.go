package ingress

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestServerHealth_503UntilReadyThen200(t *testing.T) {
	connected := false
	routesReady := false
	s := NewServer(ServerConfig{
		AppsDomain:  "apps.example.com",
		Proxy:       http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }),
		Cert:        &SelfSignedCertProvider{AppsDomain: "apps.example.com"},
		IsConnected: func() bool { return connected },
		RoutesReady: func() bool { return routesReady },
	})
	h := s.Handler()

	probe := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://_health.apps.example.com/", nil)
		req.Host = "_health.apps.example.com"
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if probe() != http.StatusServiceUnavailable {
		t.Fatal("health should be 503 before mesh+routes are ready")
	}
	connected = true
	if probe() != http.StatusServiceUnavailable {
		t.Fatal("health should still be 503 with routes not fetched")
	}
	routesReady = true
	if probe() != http.StatusOK {
		t.Fatal("health should be 200 once mesh connected and routes fetched")
	}
}

func TestServer_EndToEndTLS_HealthAndProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pod-ok")
	}))
	defer upstream.Close()

	d := &countingDialer{target: upstream.Listener.Addr().String()}
	res := &fakeResolver{routes: map[string]Route{
		"app": {Slug: "app", MeshIP: netip.MustParseAddr("100.64.0.10"), Port: 8080, Visibility: VisibilityPublic},
	}}
	proxy := NewProxy(ProxyConfig{AppsDomain: "apps.example.com", Resolver: res, DialContext: d.dial})

	cert := &SelfSignedCertProvider{AppsDomain: "apps.example.com"}
	if err := cert.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(ServerConfig{
		AppsDomain:  "apps.example.com",
		Proxy:       proxy,
		Cert:        cert,
		IsConnected: func() bool { return true },
		RoutesReady: func() bool { return true },
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, ln) }()

	addr := ln.Addr().String()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr) // ignore SNI host, dial our listener
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}

	// Health over the PUBLIC TLS listener under the reserved host.
	waitForServer(t, client, "https://_health.apps.example.com/")
	if code := doGet(t, client, "https://_health.apps.example.com/"); code != http.StatusOK {
		t.Fatalf("health over public listener = %d, want 200", code)
	}
	// A proxied public app over the same public TLS listener.
	resp, err := client.Get("https://app.apps.example.com/")
	if err != nil {
		t.Fatalf("proxied GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "pod-ok" {
		t.Fatalf("proxied response = %d %q", resp.StatusCode, string(body))
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not shut down within 5s")
	}
}

func waitForServer(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not come up within 5s")
}

func doGet(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
