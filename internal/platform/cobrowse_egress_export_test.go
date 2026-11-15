package platform

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/browseregress"
)

type RemoteEgressFixture struct {
	Manager    *CobrowseSessionManager
	Request    func(string) int
	Hits       func() int32
	Proxy      func() string
	CloseGuard func()
	Rebind     func()
}

func NewRemoteEgressFixture(t *testing.T) *RemoteEgressFixture {
	t.Helper()
	cdp := newFakeDevTools(t)
	cdp.set([2]string{"a", "https://public.test/"})
	var hits atomic.Int32
	var rebinding atomic.Bool
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Connection", "close")
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "public fixture")
	}))
	t.Cleanup(web.Close)
	g, err := browseregress.StartEgressGuard(func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "rebind.test" && rebinding.Load() {
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "8.8.8.8:80" {
			t.Errorf("unpinned dial %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, web.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	m.requireEgress = true
	m.egressFactory = func() (sessionEgress, error) { return g, nil }
	var launchedProxy string
	previous := launchCobrowseProc
	t.Cleanup(func() { launchCobrowseProc = previous })
	launchCobrowseProc = func(_, _ string, lo LaunchOptions) (*cobrowseProc, error) {
		launchedProxy = lo.ProxyServer
		return &cobrowseProc{debugPort: cdp.port, exited: make(chan struct{}), stop: func() error { return nil }}, nil
	}
	t.Cleanup(func() { m.StopAll() })
	return &RemoteEgressFixture{Manager: m, Hits: hits.Load, Proxy: func() string { return launchedProxy }, CloseGuard: g.Close, Rebind: func() { rebinding.Store(true) }, Request: func(raw string) int {
		t.Helper()
		if launchedProxy == "" {
			t.Fatal("remote launcher omitted proxy")
		}
		proxy, _ := url.Parse(launchedProxy)
		tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		client := http.Client{Transport: tr}
		resp, err := client.Get(raw)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}}
}
