package browserurl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// EgressGuard is a loopback HTTP proxy that every connection of a guarded
// co-browse browser goes through (--proxy-server, with loopback not bypassed).
// Local mode always uses one; a node-wide session uses one when the node is set
// to public-only browsing (citadel-cli#1867). It resolves each destination
// itself, refuses any address BlockedAddr rejects or that is this machine's
// own or on one of its directly attached networks (ownNet), and connects to
// the exact address it checked. So the address policy holds
// where the browser actually connects: for redirects, scripts, iframes and
// subresources, and across DNS rebinding, not only for the URL an agent asked
// to open. Plain HTTP is forwarded; HTTPS and WebSockets use CONNECT.
//
// Known gap: a destination that is the home router's own public IPv4 address
// can be answered by hairpin NAT with a port-forwarded service on the home
// network. The guard cannot tell that address from any other public one.
type EgressGuard struct {
	resolve   Resolver
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	onRefused func(scheme, host string)
	own       *ownNet
	ln        net.Listener
	srv       *http.Server
	fwd       *http.Transport

	refused atomic.Int64
	mu      sync.Mutex
	conns   map[net.Conn]struct{}

	// canaryHost is a random name the guard answers itself (VerifyRouted).
	canaryHost string
	canarySeen atomic.Bool
	// closing is set by Close; failed is closed when Serve exits without it.
	closing atomic.Bool
	failed  chan struct{}
}

// ErrEgressRefused is returned when a destination is not a public address.
var ErrEgressRefused = errors.New("destination refused by the co-browse egress guard")

// EgressGuardOptions configures StartEgressGuardWithOptions. Zero values take
// the system resolver and dialer and report nothing.
type EgressGuardOptions struct {
	Resolve Resolver
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// OnRefused, when set, is called once per refused request with the scheme
	// ("http" for a forwarded request, "https" for a CONNECT tunnel) and the
	// lower-cased host, without port, path or query. It runs on the request's
	// goroutine and must not block.
	OnRefused func(scheme, host string)
	// Interfaces lists this machine's interface addresses and on-link
	// prefixes. Nil reads the machine's own (DefaultInterfacePrefixes).
	Interfaces InterfacePrefixes
}

// StartEgressGuard listens on 127.0.0.1. resolve and dial default to the
// system resolver and dialer.
func StartEgressGuard(resolve Resolver, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*EgressGuard, error) {
	return StartEgressGuardWithOptions(EgressGuardOptions{Resolve: resolve, Dial: dial})
}

// StartEgressGuardWithOptions is StartEgressGuard with a refusal callback.
func StartEgressGuardWithOptions(o EgressGuardOptions) (*EgressGuard, error) {
	resolve, dial := o.Resolve, o.Dial
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
	var tok [12]byte
	if _, err := rand.Read(tok[:]); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("start co-browse egress guard: %w", err)
	}
	g := &EgressGuard{
		resolve: resolve, dial: dial, onRefused: o.OnRefused, own: newOwnNet(o.Interfaces),
		ln: ln, conns: map[net.Conn]struct{}{},
		canaryHost: hex.EncodeToString(tok[:]) + canarySuffix,
		failed:     make(chan struct{}),
	}
	g.fwd = &http.Transport{
		Proxy:                 nil,
		DialContext:           g.guardedDial,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	g.srv = &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		_ = g.srv.Serve(ln)
		if !g.closing.Load() {
			close(g.failed)
		}
	}()
	return g, nil
}

// canarySuffix is under the reserved .invalid TLD, so a canary request that
// bypassed the guard could never resolve anywhere.
const canarySuffix = ".egress-canary.invalid"

// CanaryURL is a URL only this guard answers. A browser that loads it has
// sent at least that request through the guard.
func (g *EgressGuard) CanaryURL() string { return "http://" + g.canaryHost + "/" }

// CanarySeen reports whether a request for CanaryURL's host reached the guard.
func (g *EgressGuard) CanarySeen() bool { return g.canarySeen.Load() }

// Failed is closed when the guard stops serving without Close being called.
// A browser behind a dead guard has no route, but its session should end.
func (g *EgressGuard) Failed() <-chan struct{} { return g.failed }

// isCanary answers a canary request itself, never dialing anything.
func (g *EgressGuard) isCanary(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.TrimSuffix(strings.ToLower(host), ".") != g.canaryHost {
		return false
	}
	g.canarySeen.Store(true)
	return true
}

// ProxyURL is the value for LaunchOptions.ProxyServer.
func (g *EgressGuard) ProxyURL() string { return "http://" + g.ln.Addr().String() }

// Refused reports how many connections were refused.
func (g *EgressGuard) Refused() int64 { return g.refused.Load() }

// Close stops the guard and drops open tunnels.
func (g *EgressGuard) Close() {
	g.closing.Store(true)
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
	if a, ok := ParseBrowserHost(host); ok {
		if BlockedAddr(a) || g.own.contains(a) {
			return nil, ErrEgressRefused
		}
		return []netip.Addr{a}, nil
	}
	if LooksNumericHost(host) {
		return nil, ErrEgressRefused
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: %v", host, err)
	}
	for _, a := range addrs {
		if BlockedAddr(a) || g.own.contains(a) {
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
	http.Error(w, "Citadel co-browse: "+http.StatusText(code), code)
}

// reportRefused passes a refusal to OnRefused with the host reduced to a
// bounded, lower-cased name or address: never a port, path or query.
func (g *EgressGuard) reportRefused(err error, scheme, host string) {
	if g.onRefused == nil || !errors.Is(err, ErrEgressRefused) {
		return
	}
	g.onRefused(scheme, logHost(host))
}

// logHost keeps only characters a host name or address literal can hold, so
// a request cannot write arbitrary text into a log line.
func logHost(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		if b.Len() >= 253 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == ':', r == '[', r == ']', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
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
	if g.isCanary(r.URL.Host) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		out.Header.Del(h)
	}
	resp, err := g.fwd.RoundTrip(out)
	if err != nil {
		g.reportRefused(err, "http", r.URL.Hostname())
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
	if g.isCanary(r.Host) {
		// A browser may upgrade the canary to HTTPS; seeing the CONNECT is
		// enough, and nothing is tunneled.
		http.Error(w, "Citadel co-browse: canary", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	upstream, err := g.guardedDial(ctx, "tcp", r.Host)
	cancel()
	if err != nil {
		host := r.Host
		if h, _, serr := net.SplitHostPort(r.Host); serr == nil {
			host = h
		}
		g.reportRefused(err, "https", host)
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
