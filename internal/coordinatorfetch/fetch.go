// Package coordinatorfetch owns bounded capability GETs from a trusted node
// configuration origin. It grants no file, job, credential or execution authority.
package coordinatorfetch

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Endpoint is a closed route set, not a URL or path supplied by a job.
type Endpoint uint8

const (
	FileFetch Endpoint = iota + 1
	MediaInput
	FileFetchPath  = "/api/shared/file-fetch"
	MediaInputPath = "/api/shared/media-input"
	MaxBytes       = int64(2 << 30)
	Timeout        = 5 * time.Minute
)

// Refusal contains only a static reason; never wrap a URL, token, body or error.
type Refusal struct{ reason string }

func (e *Refusal) Error() string  { return "coordinator fetch refused: " + e.reason }
func (e *Refusal) Reason() string { return e.reason }
func refuse(reason string) error { return &Refusal{reason: reason} }

// Hooks are trusted programmatic test seams, never values from a job. Production
// callers use zero values except a shared interface-enumeration dependency.
type Hooks struct {
	LookupIP       func(context.Context, string) ([]net.IPAddr, error)
	DialIP         func(context.Context, string, string) (net.Conn, error)
	NewTransport   func() *http.Transport
	InterfaceAddrs func() ([]net.Addr, error)
}

// Result describes the bytes written, not permission to publish them. Callers
// must verify their expected size/hash and perform their own live final gates.
type Result struct {
	Bytes  int64
	SHA256 [sha256.Size]byte
}

// Origin permits the existing FILE_FETCH canonical bare HTTPS origin only.
func Origin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Port() != "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") ||
		u.RawPath != "" || strings.ContainsAny(u.Host, "%\\") || strings.HasSuffix(u.Host, ".") ||
		(raw != "https://"+u.Host && raw != "https://"+u.Host+"/") || strings.HasSuffix(u.Host, ":") {
		return nil, refuse("untrusted_origin")
	}
	return u, nil
}

func ValidCapability(token string) bool {
	if token == "" || token == "." || token == ".." || len(token) > 4096 {
		return false
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
	} {
		if netip.MustParsePrefix(prefix).Contains(ip) {
			return false
		}
	}
	return true
}

// Transport retains real certificate validation and pins every reconnect after
// checking all DNS answers and this node's own interfaces. No proxy is used.
func Transport(host string, hooks Hooks) *http.Transport {
	lookup := hooks.LookupIP
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	dial := hooks.DialIP
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	interfaces := hooks.InterfaceAddrs
	if interfaces == nil {
		interfaces = net.InterfaceAddrs
	}
	tr := &http.Transport{}
	if hooks.NewTransport != nil {
		tr = hooks.NewTransport()
	}
	tr.Proxy = nil
	tr.DisableCompression = true
	tr.DisableKeepAlives = true
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.MaxResponseHeaderBytes = 64 << 10
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		hostname, port, err := net.SplitHostPort(address)
		if err != nil || hostname != host || port != "443" {
			return nil, refuse("destination_blocked")
		}
		ips, err := lookup(ctx, hostname)
		if err != nil || len(ips) == 0 {
			return nil, refuse("dns_failed")
		}
		addrs, err := interfaces()
		if err != nil {
			return nil, refuse("destination_blocked")
		}
		own := make(map[netip.Addr]bool, len(addrs))
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if addr, ok := netip.AddrFromSlice(ip); ok {
				own[addr.Unmap().WithZone("")] = true
			}
		}
		for _, ip := range ips {
			addr, ok := netip.AddrFromSlice(ip.IP)
			if !ok || ip.Zone != "" || !publicIP(addr) || own[addr.Unmap().WithZone("")] {
				return nil, refuse("destination_blocked")
			}
		}
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, refuse("connect_failed")
	}
	return tr
}

// Get streams at most maxBytes+1 bytes into a private caller-owned sink. A
// refusal leaves partial bytes in that sink: they must be discarded, never
// published. There are no filesystem operations, logging or automatic retries.
func Get(ctx context.Context, origin string, endpoint Endpoint, token string, maxBytes int64, sink io.Writer, hooks Hooks) (Result, error) {
	var result Result
	u, err := Origin(origin)
	if err != nil {
		return result, err
	}
	switch endpoint {
	case FileFetch:
		u.Path = FileFetchPath
	case MediaInput:
		u.Path = MediaInputPath
	default:
		return result, refuse("invalid_endpoint")
	}
	if !ValidCapability(token) {
		return result, refuse("invalid_capability")
	}
	if maxBytes < 1 || maxBytes > MaxBytes || (endpoint == MediaInput && maxBytes > 16<<20) || sink == nil {
		return result, refuse("invalid_max_bytes")
	}
	deadline, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(deadline, http.MethodGet, u.String(), nil)
	if err != nil {
		return result, refuse("invalid_capability_url")
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Authorization", "Bearer "+token)
	tr := Transport(u.Hostname(), hooks)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if deadline.Err() != nil {
			return result, refuse("cancelled_or_timeout")
		}
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return result, refusal
		}
		return result, refuse("fetch_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
		return result, refuse("capability_invalid_or_expired")
	}
	if resp.StatusCode != http.StatusOK {
		return result, refuse("unexpected_status")
	}
	if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return result, refuse("content_encoding_refused")
	}
	if resp.ContentLength > maxBytes {
		return result, refuse("size_exceeded")
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(sink, hash), io.LimitReader(resp.Body, maxBytes+1))
	if deadline.Err() != nil {
		return result, refuse("cancelled_or_timeout")
	}
	if err != nil {
		return result, refuse("transfer_failed")
	}
	if n > maxBytes {
		return result, refuse("size_exceeded")
	}
	result.Bytes = n
	copy(result.SHA256[:], hash.Sum(nil))
	return result, nil
}
