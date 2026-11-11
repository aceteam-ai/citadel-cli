package localcobrowse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EgressGuard is a loopback HTTP proxy that every local co-browse browser
// connection goes through (--proxy-server, with loopback not bypassed). It
// resolves each destination itself, refuses any address blockedAddr rejects,
// and connects to the exact address it checked. So the address policy holds
// where the browser actually connects: for redirects, scripts, iframes and
// subresources, and across DNS rebinding, not only for the URL an agent asked
// to open. Plain HTTP is forwarded; HTTPS and WebSockets use CONNECT.
type EgressGuard struct {
	resolve Resolver
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	ln      net.Listener
	srv     *http.Server
	fwd     *http.Transport

	refused atomic.Int64
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
}

// ErrEgressRefused is returned when a destination is not a public address.
var ErrEgressRefused = errors.New("destination refused by the local co-browse guard")

// StartEgressGuard listens on 127.0.0.1. resolve and dial default to the
// system resolver and dialer.
func StartEgressGuard(resolve Resolver, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*EgressGuard, error) {
	if resolve == nil {
		resolve = DefaultResolver
	}
	if dial == nil {
		d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start co-browse egress guard: %w", err)
	}
	g := &EgressGuard{resolve: resolve, dial: dial, ln: ln, conns: map[net.Conn]struct{}{}}
	g.fwd = &http.Transport{
		Proxy:                 nil,
		DialContext:           g.guardedDial,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	g.srv = &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = g.srv.Serve(ln) }()
	return g, nil
}

// ProxyURL is the value for LaunchOptions.ProxyServer.
func (g *EgressGuard) ProxyURL() string { return "http://" + g.ln.Addr().String() }

// Refused reports how many connections were refused.
func (g *EgressGuard) Refused() int64 { return g.refused.Load() }

// Close stops the guard and drops open tunnels.
func (g *EgressGuard) Close() {
	_ = g.srv.Close()
	g.fwd.CloseIdleConnections()
	g.mu.Lock()
	for c := range g.conns {
		_ = c.Close()
	}
	g.mu.Unlock()
}

// checkedAddrs resolves host and returns its addresses, refusing if any is not
// public (an attacker-controlled name that mixes public and private answers is
// refused outright).
func (g *EgressGuard) checkedAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, ErrEgressRefused
	}
	if a, ok := parseBrowserHost(host); ok {
		if blockedAddr(a) {
			return nil, ErrEgressRefused
		}
		return []netip.Addr{a}, nil
	}
	if looksNumericHost(host) {
		return nil, ErrEgressRefused
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: %v", host, err)
	}
	for _, a := range addrs {
		if blockedAddr(a) {
			return nil, ErrEgressRefused
		}
	}
	return addrs, nil
}

// guardedDial resolves, checks, and dials a checked address (never the name).
func (g *EgressGuard) guardedDial(ctx context.Context, network, hostport string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return nil, ErrEgressRefused
	}
	addrs, err := g.checkedAddrs(ctx, host)
	if err != nil {
		if errors.Is(err, ErrEgressRefused) {
			g.refused.Add(1)
		}
		return nil, err
	}
	var lastErr error
	for _, a := range addrs {
		c, err := g.dial(ctx, network, net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (g *EgressGuard) track(c net.Conn, on bool) {
	g.mu.Lock()
	if on {
		g.conns[c] = struct{}{}
	} else {
		delete(g.conns, c)
	}
	g.mu.Unlock()
}

func refuseResponse(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	if errors.Is(err, ErrEgressRefused) {
		code = http.StatusForbidden
	}
	http.Error(w, "Citadel local co-browse: "+http.StatusText(code), code)
}

// ServeHTTP handles CONNECT tunnels and absolute-URI HTTP requests.
func (g *EgressGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		g.serveConnect(w, r)
		return
	}
	if r.URL == nil || !r.URL.IsAbs() || r.URL.Scheme != "http" {
		http.Error(w, "proxy requests only", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := g.fwd.RoundTrip(out)
	if err != nil {
		refuseResponse(w, err)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (g *EgressGuard) serveConnect(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	upstream, err := g.guardedDial(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		refuseResponse(w, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "tunnel unsupported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	g.track(client, true)
	g.track(upstream, true)
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	done := make(chan struct{}, 2)
	go func() {
		if buf != nil && buf.Reader.Buffered() > 0 {
			_, _ = io.CopyN(upstream, buf, int64(buf.Reader.Buffered()))
		}
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
	g.track(client, false)
	g.track(upstream, false)
}
