package containerjobs

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}

type HTTPTransport struct{ client *http.Client }

func NewHTTPTransport() *HTTPTransport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return capabilityTransport(net.DefaultResolver.LookupIPAddr, dialer.DialContext, net.InterfaceAddrs)
}
func capabilityTransport(lookup func(context.Context, string) ([]net.IPAddr, error), dial func(context.Context, string, string) (net.Conn, error), locals func() ([]net.Addr, error)) *HTTPTransport {
	tr := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableKeepAlives: true}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, ErrInvalid
		}
		ips, err := lookup(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, ErrUnavailable
		}
		local, err := locals()
		if err != nil {
			return nil, ErrUnavailable
		}
		own := map[netip.Addr]bool{}
		for _, entry := range local {
			prefix, err := netip.ParsePrefix(entry.String())
			if err != nil {
				return nil, ErrUnavailable
			}
			own[prefix.Addr().Unmap()] = true
		}
		for _, ip := range ips {
			a, ok := netip.AddrFromSlice(ip.IP)
			if !ok || ip.Zone != "" || !publicIP(a) || own[a.Unmap()] {
				return nil, ErrInvalid
			}
		}
		for _, ip := range ips {
			c, err := dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return c, nil
			}
		}
		return nil, ErrUnavailable
	}
	return &HTTPTransport{client: &http.Client{Transport: tr, Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}}
}
func (t *HTTPTransport) Put(ctx context.Context, url string, b []byte) error {
	if !validURL(url) || len(b) > ChunkLimit+28 {
		return ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(b))
	if err != nil {
		return ErrInvalid
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := t.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrUnavailable
	}
	return nil
}
func (t *HTTPTransport) Get(ctx context.Context, url string, limit int64) ([]byte, error) {
	if !validURL(url) || limit <= 28 || limit > ChunkLimit+28 {
		return nil, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, ErrInvalid
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > limit {
		return nil, ErrInvalid
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, ErrBounds
	}
	return b, nil
}
