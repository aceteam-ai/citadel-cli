package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

// FileFetchEndpointPrefix is the dedicated, read-only capability endpoint.
// The coordinator owns HMAC verification, exact org/node/document/object/hash/
// size binding, the ten-minute maximum capability lifetime, and mint/fetch audit.
// Keep this literal in sync with the coordinator; this is not a proxy allowlist.
const FileFetchEndpointPrefix = "/api/shared/file-fetch/"

const (
	fileFetchHardMaxBytes  int64 = 2 << 30
	fileFetchTimeout             = 5 * time.Minute
	fileFetchMaxTokenBytes       = 16 << 10
)

// FileFetchHandler stages a capability-bound document into the workspace.
// Origin must come from trusted node configuration, never the job payload.
type FileFetchHandler struct {
	WorkspaceDir string
	Origin       func() string
	Disabled     bool
	Enabled      func() bool

	// Private test seams retain the real HTTP transport and filesystem path.
	lookupIP     func(context.Context, string) ([]net.IPAddr, error)
	dialIP       func(context.Context, string, string) (net.Conn, error)
	newTransport func() *http.Transport
}

// NewFileFetchHandler creates a handler with no implicit trusted destination.
func NewFileFetchHandler(workspace string, origin func() string) *FileFetchHandler {
	return &FileFetchHandler{WorkspaceDir: workspace, Origin: origin}
}

type fileFetchError struct{ reason string }

func (e *fileFetchError) Error() string {
	// Only static reasons are used. Never return URL parser, transport, response
	// body or filesystem errors: they can contain tokens or node-local paths.
	return fmt.Sprintf(`{"reason":%q,"message":"FILE_FETCH refused"}`, e.reason)
}

func fileFetchRefusal(reason string) error {
	return &fileFetchError{reason: reason}
}

func fileFetchOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Port() != "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") ||
		u.RawPath != "" || strings.ContainsAny(u.Host, "%\\") || strings.HasSuffix(u.Host, ".") ||
		(raw != "https://"+u.Host && raw != "https://"+u.Host+"/") || strings.HasSuffix(u.Host, ":") {
		return nil, fileFetchRefusal("untrusted_origin")
	}
	return u, nil
}

// Configured reports whether an exact HTTPS origin is configured. Worker
// capability advertisement also requires a workspace and live Files permission.
func (h *FileFetchHandler) Configured() bool {
	if h.Origin == nil {
		return false
	}
	_, err := fileFetchOrigin(h.Origin())
	return err == nil
}

func (h *FileFetchHandler) validateURL(raw string) (*url.URL, error) {
	if h.Origin == nil {
		return nil, fileFetchRefusal("untrusted_origin")
	}
	origin, err := fileFetchOrigin(h.Origin())
	if err != nil {
		return nil, err
	}
	if len(raw) > fileFetchMaxTokenBytes+len(FileFetchEndpointPrefix)+len(origin.Host)+8 {
		return nil, fileFetchRefusal("invalid_capability_url")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != origin.Scheme || u.Host != origin.Host ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || strings.Contains(raw, "%") ||
		!strings.HasPrefix(u.Path, FileFetchEndpointPrefix) {
		return nil, fileFetchRefusal("invalid_capability_url")
	}
	token := strings.TrimPrefix(u.Path, FileFetchEndpointPrefix)
	if token == "" || token == "." || token == ".." || len(token) > fileFetchMaxTokenBytes {
		return nil, fileFetchRefusal("invalid_capability_url")
	}
	for _, c := range token {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return nil, fileFetchRefusal("invalid_capability_url")
		}
	}
	// Reject a trailing '#' as well, even though Parse drops its empty fragment.
	if raw != "https://"+origin.Host+FileFetchEndpointPrefix+token {
		return nil, fileFetchRefusal("invalid_capability_url")
	}
	return u, nil
}

// fileFetchPublicIP excludes special-purpose ranges as well as RFC1918/ULA,
// metadata, loopback, multicast and the mesh. IPv6 transition/NAT64 addresses
// are refused so they cannot encode a destination that bypasses IPv4 policy.
func fileFetchPublicIP(ip netip.Addr) bool {
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

func (h *FileFetchHandler) transport(host string) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	lookup := h.lookupIP
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	dial := h.dialIP
	if dial == nil {
		dial = dialer.DialContext
	}
	tr := &http.Transport{}
	if h.newTransport != nil {
		tr = h.newTransport()
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
			return nil, fileFetchRefusal("destination_blocked")
		}
		ips, err := lookup(ctx, hostname)
		if err != nil || len(ips) == 0 {
			return nil, fileFetchRefusal("dns_failed")
		}
		own, err := ownInterfaceAddrs()
		if err != nil {
			return nil, fileFetchRefusal("destination_blocked")
		}
		// Validate ALL answers before any connect; pin actual numeric addresses.
		// Re-resolution and these checks run on every transport reconnect.
		for _, ip := range ips {
			addr, ok := netip.AddrFromSlice(ip.IP)
			if !ok || ip.Zone != "" || !fileFetchPublicIP(addr) || own.has(addr) {
				return nil, fileFetchRefusal("destination_blocked")
			}
		}
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
		}
		return nil, fileFetchRefusal("connect_failed")
	}
	return tr
}

// Execute accepts only url, dest_path, expected_sha256 and max_bytes (plus
// dispatch metadata ignored by the handler). GET, headers and policy are fixed.
// The opaque URL's signature and expiry are checked by the dedicated endpoint.
func (h *FileFetchHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	if (h.Enabled != nil && !h.Enabled()) || (h.Enabled == nil && h.Disabled) {
		return nil, fileFetchRefusal("files_disabled")
	}
	if ctx.Context().Err() != nil || ctx.CancelRequestedNow() {
		return nil, fileFetchRefusal("cancelled_or_timeout")
	}
	u, err := h.validateURL(job.Payload["url"])
	if err != nil {
		return nil, err
	}
	dest := job.Payload["dest_path"]
	if dest == "." || !filepath.IsLocal(dest) || strings.ContainsAny(dest, "\\:\x00") || filepath.ToSlash(filepath.Clean(dest)) != dest {
		return nil, fileFetchRefusal("invalid_dest_path")
	}
	wantHash, err := hex.DecodeString(job.Payload["expected_sha256"])
	if err != nil || len(wantHash) != sha256.Size {
		return nil, fileFetchRefusal("invalid_sha256")
	}
	maxBytes, err := strconv.ParseInt(job.Payload["max_bytes"], 10, 64)
	if err != nil || maxBytes <= 0 || maxBytes > fileFetchHardMaxBytes {
		return nil, fileFetchRefusal("invalid_max_bytes")
	}
	if h.WorkspaceDir == "" {
		return nil, fileFetchRefusal("workspace_unavailable")
	}
	workspace, err := filepath.Abs(h.WorkspaceDir)
	if err != nil {
		return nil, fileFetchRefusal("workspace_unavailable")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, fileFetchRefusal("workspace_unavailable")
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return nil, fileFetchRefusal("invalid_dest_path")
	}
	if info, err := root.Lstat(dest); err == nil && !info.Mode().IsRegular() {
		return nil, fileFetchRefusal("invalid_dest_path")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fileFetchRefusal("invalid_dest_path")
	}

	deadline, cancel := context.WithTimeout(ctx.Context(), fileFetchTimeout)
	defer cancel()
	if ctx.CancelRequested != nil {
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-deadline.Done():
					return
				case <-ticker.C:
					if ctx.CancelRequestedNow() {
						cancel()
						return
					}
				}
			}
		}()
	}
	req, err := http.NewRequestWithContext(deadline, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fileFetchRefusal("invalid_capability_url")
	}
	req.Header.Set("Accept-Encoding", "identity")
	tr := h.transport(u.Hostname())
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if deadline.Err() != nil {
			return nil, fileFetchRefusal("cancelled_or_timeout")
		}
		var refusal *fileFetchError
		if errors.As(err, &refusal) {
			return nil, refusal
		}
		return nil, fileFetchRefusal("fetch_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
		return nil, fileFetchRefusal("capability_invalid_or_expired")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fileFetchRefusal("unexpected_status")
	}
	if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return nil, fileFetchRefusal("content_encoding_refused")
	}
	if resp.ContentLength > maxBytes {
		return nil, fileFetchRefusal("size_exceeded")
	}

	tempName := ".file-fetch-" + rand.Text()
	// Keep the temporary entry directly under the workspace descriptor. All
	// operations, including Rename's destination resolution, use this SAME root;
	// a concurrently replaced ancestor cannot redirect publication or cleanup.
	temp, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fileFetchRefusal("write_failed")
	}
	defer root.Remove(tempName)
	defer temp.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		if deadline.Err() != nil {
			return nil, fileFetchRefusal("cancelled_or_timeout")
		}
		return nil, fileFetchRefusal("transfer_failed")
	}
	if n > maxBytes {
		return nil, fileFetchRefusal("size_exceeded")
	}
	sum := hash.Sum(nil)
	if !bytes.Equal(sum, wantHash) {
		return nil, fileFetchRefusal("sha256_mismatch")
	}
	if err := temp.Sync(); err != nil {
		return nil, fileFetchRefusal("write_failed")
	}
	if err := temp.Close(); err != nil {
		return nil, fileFetchRefusal("write_failed")
	}
	if deadline.Err() != nil || ctx.CancelRequestedNow() {
		return nil, fileFetchRefusal("cancelled_or_timeout")
	}
	if (h.Enabled != nil && !h.Enabled()) || (h.Enabled == nil && h.Disabled) {
		return nil, fileFetchRefusal("files_disabled")
	}
	if err := root.Rename(tempName, dest); err != nil {
		return nil, fileFetchRefusal("publish_failed")
	}
	// No URL, token, payload, response headers/body or destination in logs.
	ctx.Log("info", "FILE_FETCH completed (%d bytes)", n)
	return json.Marshal(map[string]any{"path": filepath.Join(workspace, dest), "bytes": n, "sha256": hex.EncodeToString(sum)})
}

// Capabilities enables the worker's cooperative cancellation watch; cancellation
// aborts the current transfer and leaves any existing destination untouched.
func (*FileFetchHandler) Capabilities() JobCapabilities {
	return JobCapabilities{CooperativeCancel: true}
}

var _ JobHandler = (*FileFetchHandler)(nil)
