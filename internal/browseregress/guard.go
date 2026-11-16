package browseregress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/aceteam-ai/citadel-cli/internal/browserurl"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EgressGuard is a loopback HTTP proxy that every guarded co-browse browser
// connection goes through (--proxy-server, with loopback not bypassed). It
// resolves each destination itself, refuses any address blockedAddr rejects,
// and connects to the exact address it checked. So the address policy holds
// where the browser actually connects: for redirects, scripts, iframes and
// subresources, and across DNS rebinding, not only for the URL an agent asked
// to open. Plain HTTP is forwarded; HTTPS and WebSockets use CONNECT.
type EgressGuard struct {
	resolve browserurl.Resolver
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	ln      net.Listener
	srv     *http.Server
	fwd     *http.Transport

	refused        atomic.Int64
	privateRefused atomic.Int64
	active         atomic.Bool
	mu             sync.Mutex
	conns          map[net.Conn]struct{}
}

// ErrEgressRefused is returned when a destination is not a public address.
var ErrEgressRefused = errors.New("destination refused by the co-browse guard")

// StartEgressGuard listens on 127.0.0.1. resolve and dial default to the
// system resolver and dialer.
func StartEgressGuard(resolve browserurl.Resolver, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*EgressGuard, error) {
	if resolve == nil {
		resolve = browserurl.DefaultResolver
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
	g.active.Store(true)
	go func() { _ = g.srv.Serve(ln); g.Close() }()
	return g, nil
}

// ProxyURL is the value for LaunchOptions.ProxyServer.
func (g *EgressGuard) ProxyURL() string { return "http://" + g.ln.Addr().String() }

// Refused reports how many connections were refused.
func (g *EgressGuard) Refused() int64 { return g.refused.Load() }

// Active reports whether the in-process proxy is serving.
func (g *EgressGuard) Active() bool { return g.active.Load() }

// PrivateRefused counts requests blocked pending a future authenticated O5 grant.
func (g *EgressGuard) PrivateRefused() int64 { return g.privateRefused.Load() }

// ErrPrivateNetworkApprovalRequired never authorizes a private dial. No approval
// bypass is implemented: a future O5 protocol must bind grants to the owning org.
var ErrPrivateNetworkApprovalRequired = fmt.Errorf("%w: private network access requires approval", ErrEgressRefused)

// Close stops the guard and drops open tunnels.
func (g *EgressGuard) Close() {
	g.active.Store(false)
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
// privateAddress distinguishes O5 ranges from addresses that remain forbidden.
var privateCGNAT = netip.MustParsePrefix("100.64.0.0/10")
var publicNAT64 = netip.MustParsePrefix("64:ff9b::/96")
var metadataIPv6 = netip.MustParseAddr("fd00:ec2::254")
var metadataCGNAT = netip.MustParseAddr("100.100.100.200")

func normalizedAddress(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if publicNAT64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return a
}

func metadataAddress(a netip.Addr) bool {
	a = normalizedAddress(a)
	return a == metadataIPv6 || a == metadataCGNAT
}

func privateAddress(a netip.Addr) bool {
	a = a.Unmap()
	if publicNAT64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return a.IsPrivate() || privateCGNAT.Contains(a)
}

func privateName(host string) bool {
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".internal", ".home.arpa", ".localdomain"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func (g *EgressGuard) checkedAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" || host == "metadata.goog" || host == "instance-data.ec2.internal" {
		return nil, ErrEgressRefused
	}
	if a, ok := browserurl.ParseBrowserHost(host); ok {
		if metadataAddress(a) {
			return nil, ErrEgressRefused
		}
		if privateAddress(a) {
			return nil, ErrPrivateNetworkApprovalRequired
		}
		if browserurl.BlockedAddr(a) {
			return nil, ErrEgressRefused
		}
		return []netip.Addr{a}, nil
	}
	if browserurl.LooksNumericHost(host) {
		return nil, ErrEgressRefused
	}
	if privateName(host) {
		return nil, ErrPrivateNetworkApprovalRequired
	}
	addrs, err := g.resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("resolve destination: %w", ErrEgressRefused)
	}
	private := false
	for _, a := range addrs {
		if metadataAddress(a) {
			return nil, ErrEgressRefused
		}
		if privateAddress(a) {
			private = true
			continue
		}
		if browserurl.BlockedAddr(a) {
			return nil, ErrEgressRefused
		}
	}
	if private {
		return nil, ErrPrivateNetworkApprovalRequired
	}
	return addrs, nil
}

// CheckURL refuses an explicit navigation before the browser starts or acts.
// The proxy independently checks and pins every actual network destination.
func (g *EgressGuard) CheckURL(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "about:blank") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" {
		return ErrEgressRefused
	}
	_, err = g.checkedAddrs(ctx, u.Hostname())
	return err
}

// recordRefusal keeps proxy-level counters consistent whether policy rejects a
// request before pooled forwarding or rejects a new numeric-address dial.
func (g *EgressGuard) recordRefusal(err error) {
	if errors.Is(err, ErrEgressRefused) {
		g.refused.Add(1)
	}
	if errors.Is(err, ErrPrivateNetworkApprovalRequired) {
		g.privateRefused.Add(1)
	}
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
		g.recordRefusal(err)
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

func (g *EgressGuard) register(client, upstream net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.Active() {
		_ = client.Close()
		_ = upstream.Close()
		return false
	}
	g.conns[client] = struct{}{}
	g.conns[upstream] = struct{}{}
	return true
}
func (g *EgressGuard) forget(client, upstream net.Conn) {
	g.mu.Lock()
	delete(g.conns, client)
	delete(g.conns, upstream)
	g.mu.Unlock()
}

func refuseResponse(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	if errors.Is(err, ErrEgressRefused) {
		code = http.StatusForbidden
	}
	if errors.Is(err, ErrPrivateNetworkApprovalRequired) {
		http.Error(w, "Citadel co-browse: private network approval required", code)
		return
	}
	http.Error(w, "Citadel co-browse: "+http.StatusText(code), code)
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
	// Revalidate current destination policy before every plain-HTTP request,
	// even if the transport will reuse a previously checked public connection.
	// New connections still independently resolve, check and pin a numeric IP in
	// guardedDial, so this check never authorizes a later unchecked dial.
	checkCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	err := g.CheckURL(checkCtx, r.URL.String())
	cancel()
	if err != nil {
		g.recordRefusal(err)
		refuseResponse(w, err)
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
	if !g.register(client, upstream) {
		return
	}
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
	g.forget(client, upstream)
}
