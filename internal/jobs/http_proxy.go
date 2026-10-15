// internal/jobs/http_proxy.go
package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// defaultHTTPProxyLocalPorts are the only ports HTTP_PROXY may reach on the
// node's own host (loopback or one of its own interface addresses), as
// requested by citadel-cli#1326 for the WeChat bridge port (:8000). The
// documented deployment reaches the bridge on a separate VM over the LAN or
// mesh, which this exception does not affect; it only matters if a bridge is
// co-located on the node host.
var defaultHTTPProxyLocalPorts = map[int]bool{8000: true}

// errHTTPProxyDestinationBlocked marks a refused destination.
var errHTTPProxyDestinationBlocked = errors.New("destination not allowed")

type HTTPProxyHandler struct {
	// localPorts overrides defaultHTTPProxyLocalPorts. Test seam only; the
	// zero value uses the default.
	localPorts map[int]bool
}

func (h *HTTPProxyHandler) allowedLocalPorts() map[int]bool {
	if h.localPorts != nil {
		return h.localPorts
	}
	return defaultHTTPProxyLocalPorts
}

// httpProxyDestinationAllowed decides whether HTTP_PROXY may connect to ip:port.
//
// Node-local destinations are loopback and the node's own interface addresses
// (own): they reach the node's own services, which may bind every interface,
// so they are refused except on an allowlisted port. Link-local (unicast and
// multicast), interface-local multicast and unspecified addresses are always
// refused. Everything else, including the AceTeam Network range 100.64.0.0/10
// where mesh peers and the WeChat VM live, is allowed.
func httpProxyDestinationAllowed(ip netip.Addr, port int, localPorts map[int]bool, own map[netip.Addr]bool) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.IsLoopback() || own[ip] {
		return localPorts[port]
	}
	return true
}

// guardedDialControl runs after name resolution, immediately before each
// connect, with the exact address being dialed. Checking here pins the check
// to the dialed IP, so a DNS answer that changes between a pre-check and the
// connect (rebinding) cannot slip past it.
func guardedDialControl(localPorts map[int]bool, own map[netip.Addr]bool) func(network, address string, _ syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		host, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("%w: %v", errHTTPProxyDestinationBlocked, err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("%w: unparseable address %q", errHTTPProxyDestinationBlocked, host)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return fmt.Errorf("%w: unparseable port %q", errHTTPProxyDestinationBlocked, portStr)
		}
		if !httpProxyDestinationAllowed(ip, port, localPorts, own) {
			return fmt.Errorf("%w: %s port %d", errHTTPProxyDestinationBlocked, ip.Unmap(), port)
		}
		return nil
	}
}

// newGuardedHTTPClient builds the HTTP_PROXY client: every connect is checked
// by guardedDialControl, redirects are never followed (the 3xx itself is
// returned to the caller), and no environment proxy is used, so the guarded
// dial is the only path out. The node's own interface addresses are
// snapshotted once per client (one client per job).
func newGuardedHTTPClient(localPorts map[int]bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guardedDialControl(localPorts, ownInterfaceAddrs())}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (h *HTTPProxyHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	rawURL, ok := job.Payload["url"]
	if !ok {
		return nil, fmt.Errorf("job payload missing 'url' field")
	}
	method := job.Payload["method"]
	if method == "" {
		method = "GET"
	}

	// Fail closed on anything but an absolute http(s) URL with a host. The
	// destination address itself is checked at connect time.
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q (only http and https)", errHTTPProxyDestinationBlocked, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("%w: url has no host", errHTTPProxyDestinationBlocked)
	}

	ctx.Log("info", "     - [Job %s] HTTP %s %s", job.ID, method, rawURL)

	var bodyReader io.Reader
	if body, ok := job.Payload["body"]; ok && body != "" {
		bodyReader = bytes.NewBufferString(body)
	}

	req, err := http.NewRequestWithContext(ctx.Context(), method, parsed.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if headersJSON, ok := job.Payload["headers"]; ok && headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			return nil, fmt.Errorf("failed to parse headers: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}

	if req.Header.Get("Content-Type") == "" && bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := newGuardedHTTPClient(h.allowedLocalPorts())
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, errHTTPProxyDestinationBlocked) {
			return nil, fmt.Errorf("request refused: %w", err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("request cancelled: %w", err)
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	result := map[string]any{
		"status_code": resp.StatusCode,
		"body":        string(respBody),
	}
	return json.Marshal(result)
}
