// internal/jobs/web_fetch.go
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// WEB_FETCH is the native successor to HTTP_PROXY (aceteam#5995 phase 2,
// unblocks aceteam#5783). It performs one outbound HTTP request from the
// node's egress and returns a structured result. Over HTTP_PROXY it adds:
//
//   - real response headers (a curated subset) instead of none,
//   - an explicit `truncated` flag instead of silently cutting at the byte cap,
//   - `final_url` after redirects,
//   - SSRF guards (HTTP_PROXY had none): loopback / link-local / cloud-metadata
//     are always refused; private / CGNAT-mesh ranges are refused unless the
//     caller opts in with `allow_private`.
//
// The wire contract mirrors the central web_fetch guards in
// python-backend/routes/aceteam_mcp_web.py so both egress paths behave the same.
const (
	webFetchDefaultMaxBytes = 2 << 20 // 2 MB default body cap
	webFetchHardMaxBytes    = 8 << 20 // 8 MB ceiling a caller may request
	webFetchDefaultTimeout  = 30 * time.Second
	webFetchMaxTimeout      = 55 * time.Second // under the backend's 60s node deadline
	webFetchDialTimeout     = 10 * time.Second
	webFetchMaxRedirects    = 10
	webFetchUserAgent       = "AceTeam-web_fetch/1.0 (+https://aceteam.ai)"
)

// webFetchRelayResponseHeaders is the curated set of response headers relayed
// back. Keeping it small avoids leaking hop-by-hop / transport headers; the
// central path only needs content-type + final_url to extract readable text.
var webFetchRelayResponseHeaders = []string{
	"content-type",
	"content-length",
	"content-disposition",
	"content-language",
	"etag",
	"last-modified",
	"location",
}

// cgnatV4 is RFC 6598 shared address space (100.64.0.0/10), which is also the
// Headscale mesh range -- refused by default so a shared node can't be aimed at
// the mesh.
var cgnatV4 = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// errBlockedDestination is deliberately generic: it never echoes the resolved
// IP so a blocked-destination error can't be used to probe internal topology.
var errBlockedDestination = fmt.Errorf("destination address is not allowed")

type WebFetchHandler struct {
	// allowIP overrides the SSRF IP policy. Production leaves it nil and uses
	// wfAssertIPAllowed; tests inject a policy that permits loopback so an
	// httptest server (127.0.0.1) can exercise the real HTTP path.
	allowIP func(ip net.IP, allowPrivate bool) error
}

func (h *WebFetchHandler) assertIP(ip net.IP, allowPrivate bool) error {
	if h.allowIP != nil {
		return h.allowIP(ip, allowPrivate)
	}
	return wfAssertIPAllowed(ip, allowPrivate)
}

func (h *WebFetchHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	rawURL, ok := job.Payload["url"]
	if !ok || rawURL == "" {
		return nil, fmt.Errorf("job payload missing 'url' field")
	}
	method := job.Payload["method"]
	if method == "" {
		method = http.MethodGet
	}

	allowPrivate := wfBool(job.Payload["allow_private"], false)
	followRedirects := wfBool(job.Payload["follow_redirects"], true)
	maxBytes := wfClampInt(wfInt(job.Payload["max_bytes"], webFetchDefaultMaxBytes), 1024, webFetchHardMaxBytes)
	timeout := wfClampDuration(
		wfDurationSeconds(job.Payload["timeout"], webFetchDefaultTimeout),
		time.Second, webFetchMaxTimeout,
	)

	// Fast, clear failure for an obviously-blocked target. The dialer re-checks
	// every resolved IP it actually dials (and every redirect hop), so this
	// pre-check is a convenience, not the security boundary.
	if err := h.assertURLHostAllowed(rawURL, allowPrivate); err != nil {
		return nil, err
	}

	ctx.Log("info", "     - [Job %s] WEB_FETCH %s %s", job.ID, method, rawURL)

	var bodyReader io.Reader
	if body := job.Payload["body"]; body != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequest(method, rawURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if headersJSON := job.Payload["headers"]; headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			return nil, fmt.Errorf("failed to parse headers: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", webFetchUserAgent)
	}
	if req.Header.Get("Content-Type") == "" && bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: h.guardedTransport(allowPrivate),
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if !followRedirects {
				return http.ErrUseLastResponse
			}
			if len(via) >= webFetchMaxRedirects {
				return fmt.Errorf("stopped after %d redirects", webFetchMaxRedirects)
			}
			// DialContext guards the IP; the scheme is guarded here so a
			// redirect can't downgrade to file://, gopher://, etc.
			if r.URL.Scheme != "http" && r.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-http(s) scheme %q", r.URL.Scheme)
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap so "exactly at the cap" is distinguishable
	// from "truncated" -- HTTP_PROXY's silent io.LimitReader could not tell.
	limited, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	truncated := false
	if len(limited) > maxBytes {
		limited = limited[:maxBytes]
		truncated = true
	}

	headers := map[string]string{}
	for _, name := range webFetchRelayResponseHeaders {
		if v := resp.Header.Get(name); v != "" {
			headers[name] = v
		}
	}

	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	result := map[string]any{
		"status_code": resp.StatusCode,
		"final_url":   finalURL,
		"headers":     headers,
		"body":        string(limited),
		"truncated":   truncated,
	}
	return json.Marshal(result)
}

// wfGuardedTransport returns an http.Transport whose DialContext resolves the
// target host, rejects every blocked IP, and dials only a validated address.
// Because Go opens a fresh connection per redirect hop, this guards every hop
// and closes the DNS-rebinding TOCTOU (the IP validated is the IP dialed).
// Proxy is left nil so this node's egress is authoritative and the SSRF guard
// can't be bypassed by an ambient HTTP(S)_PROXY env var.
func (h *WebFetchHandler) guardedTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: webFetchDialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("dns resolution failed: %w", err)
			}
			var chosen *net.IPAddr
			for i := range ips {
				if err := h.assertIP(ips[i].IP, allowPrivate); err != nil {
					return nil, err
				}
				if chosen == nil {
					chosen = &ips[i]
				}
			}
			if chosen == nil {
				return nil, fmt.Errorf("no addresses for host %q", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(chosen.IP.String(), port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// wfAssertURLHostAllowed validates the scheme and (for literal-IP hosts) the
// destination IP up front. Hostnames are resolved best-effort here; the dialer
// re-validates the IPs it actually dials, so a DNS error here is non-fatal.
func (h *WebFetchHandler) assertURLHostAllowed(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http(s) URLs are allowed")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url has no host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return h.assertIP(ip, allowPrivate)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// Let the dialer surface DNS errors uniformly rather than duplicating them.
		return nil
	}
	for _, ip := range ips {
		if err := h.assertIP(ip, allowPrivate); err != nil {
			return err
		}
	}
	return nil
}

// wfAssertIPAllowed is the SSRF policy. Loopback (127.0.0.0/8, ::1), link-local
// (169.254.0.0/16 incl. the 169.254.169.254 metadata endpoint, fe80::/10),
// unspecified (0.0.0.0, ::) and multicast are ALWAYS refused. Private/RFC1918
// (10/8, 172.16/12, 192.168/16), ULA (fc00::/7) and CGNAT/mesh (100.64/10) are
// refused unless allowPrivate is set -- fetching your own intranet from your own
// node is a sovereignty feature, but opt-in so a shared node isn't a LAN pivot.
func wfAssertIPAllowed(ip net.IP, allowPrivate bool) error {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return errBlockedDestination
	}
	if allowPrivate {
		return nil
	}
	if ip.IsPrivate() || wfIsCGNAT(ip) {
		return errBlockedDestination
	}
	return nil
}

func wfIsCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && cgnatV4.Contains(v4)
}

func wfBool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func wfInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func wfDurationSeconds(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return def
	}
	return time.Duration(f * float64(time.Second))
}

func wfClampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func wfClampDuration(v, lo, hi time.Duration) time.Duration {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
