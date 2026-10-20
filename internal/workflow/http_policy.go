package workflow

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

const HTTPPolicyFile = "workflow-http-policy.yaml"
const maxHTTPPolicyBytes = 16 << 10

// HTTPPolicy is immutable after executor construction. Its zero value permits
// only public destinations. Graphs, inputs and run payloads cannot populate it.
// Resolver/dial seams are private so tests can exercise the production guard
// without adding a production localhost exception or contacting public hosts.
type HTTPPolicy struct {
	prefixes []netip.Prefix
	hosts    map[string]bool
	loadErr  error
	resolve  func(context.Context, string) ([]netip.Addr, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

type httpAllowEntry struct {
	CIDR string `yaml:"cidr"`
	Host string `yaml:"host"`
}

var privateHTTPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("fc00::/7"),
}

// netip.IsGlobalUnicast deliberately includes some documentation/reserved
// space. Classify it explicitly rather than treating that API as proof of
// public routability (matching the backend's stricter ipaddress.is_global).
var reservedHTTPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

var compatibleHTTPPrefix = netip.MustParsePrefix("::/96")
var publicHTTPv6Prefix = netip.MustParsePrefix("2000::/3")
var siteLocalHTTPPrefix = netip.MustParsePrefix("fec0::/10")
var zeroHTTPPrefix = netip.MustParsePrefix("0.0.0.0/8")
var meshLocalHTTPAddress = netip.MustParseAddr("100.100.100.100")

func normalizeHTTPAddress(ip netip.Addr) netip.Addr {
	ip = ip.Unmap()
	if ip.Is6() && !ip.IsUnspecified() && !ip.IsLoopback() && compatibleHTTPPrefix.Contains(ip) {
		b := ip.As16()
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	return ip
}

func httpAddressClass(ip netip.Addr) (class string, hard bool) {
	if !ip.IsValid() || ip.Zone() != "" {
		return "invalid address", true
	}
	ip = normalizeHTTPAddress(ip)
	switch {
	case ip.IsLoopback():
		return "loopback address", true
	case ip.IsUnspecified() || zeroHTTPPrefix.Contains(ip):
		return "unspecified address", true
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "link-local address", true
	case ip.IsMulticast():
		return "multicast address", true
	case ip == netip.MustParseAddr("255.255.255.255"):
		return "broadcast address", true
	case ip == meshLocalHTTPAddress:
		return "node-local service address", true
	case siteLocalHTTPPrefix.Contains(ip):
		return "site-local address", true
	}
	for _, prefix := range privateHTTPPrefixes {
		if prefix.Contains(ip) {
			return "private address", false
		}
	}
	for _, prefix := range reservedHTTPPrefixes {
		if prefix.Contains(ip) {
			return "reserved address", false
		}
	}
	if !ip.IsGlobalUnicast() || (ip.Is6() && !publicHTTPv6Prefix.Contains(ip)) {
		return "reserved address", false
	}
	return "", false
}

func canonicalHTTPHost(host string) (string, error) {
	if host == "" || len(host) > 253 || strings.TrimSpace(host) != host {
		return "", errors.New("invalid HTTP host")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return "", errors.New("scoped HTTP address is not allowed")
		}
		return normalizeHTTPAddress(ip).String(), nil
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid HTTP host")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("HTTP hosts require ASCII DNS or an IP literal")
			}
		}
	}
	return host, nil
}

// LoadHTTPPolicy reads only the explicitly supplied machine-local directory.
// Missing means public-only; every other error is retained as a refusal for
// HTTP nodes, not silently converted into permissive defaults. No file is written.
func LoadHTTPPolicy(nodeConfigDir string) HTTPPolicy {
	refuse := func() HTTPPolicy {
		return HTTPPolicy{loadErr: errors.New("HTTP policy unreadable or invalid")}
	}
	if nodeConfigDir == "" {
		return refuse()
	}
	root, err := os.OpenRoot(nodeConfigDir)
	if errors.Is(err, os.ErrNotExist) {
		return HTTPPolicy{}
	}
	if err != nil {
		return refuse()
	}
	defer root.Close()
	before, err := root.Lstat(HTTPPolicyFile)
	if errors.Is(err, os.ErrNotExist) {
		return HTTPPolicy{}
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxHTTPPolicyBytes {
		return refuse()
	}
	f, err := root.Open(HTTPPolicyFile)
	if err != nil {
		return refuse()
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return refuse()
	}
	data, err := io.ReadAll(io.LimitReader(f, maxHTTPPolicyBytes+1))
	if err != nil || len(data) > maxHTTPPolicyBytes {
		return refuse()
	}
	policy, err := parseHTTPPolicy(data)
	if err != nil {
		return refuse()
	}
	return policy
}

func parseHTTPPolicy(data []byte) (HTTPPolicy, error) {
	invalid := errors.New("invalid HTTP policy")
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil {
		return HTTPPolicy{}, invalid
	}
	// An explicit allow sequence is required; YAML null/scalars must not be
	// silently decoded into a zero-value configuration.
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return HTTPPolicy{}, invalid
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode || len(root.Content) != 2 || root.Content[0].Value != "allow" || root.Content[1].Kind != yaml.SequenceNode {
		return HTTPPolicy{}, invalid
	}
	budget := 4096
	if !validHTTPPolicyYAML(&document, 0, &budget) {
		return HTTPPolicy{}, invalid
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return HTTPPolicy{}, invalid
	}
	var config struct {
		Allow []httpAllowEntry `yaml:"allow"`
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(&config); err != nil || len(config.Allow) > 64 {
		return HTTPPolicy{}, invalid
	}
	policy := HTTPPolicy{hosts: make(map[string]bool)}
	for _, entry := range config.Allow {
		if (entry.CIDR == "") == (entry.Host == "") {
			return HTTPPolicy{}, invalid
		}
		if entry.Host != "" {
			host, err := canonicalHTTPHost(entry.Host)
			if err != nil {
				return HTTPPolicy{}, invalid
			}
			if ip, err := netip.ParseAddr(host); err == nil {
				if _, hard := httpAddressClass(ip); hard {
					return HTTPPolicy{}, invalid
				}
			}
			policy.hosts[host] = true
			continue
		}
		prefix, err := netip.ParsePrefix(entry.CIDR)
		if err != nil || prefix.Addr().Zone() != "" {
			return HTTPPolicy{}, invalid
		}
		address := normalizeHTTPAddress(prefix.Addr())
		bits := prefix.Bits()
		if address.Is4() && prefix.Addr().Is6() {
			bits -= 96
		}
		if bits < 0 {
			return HTTPPolicy{}, invalid
		}
		prefix = netip.PrefixFrom(address, bits).Masked()
		if _, hard := httpAddressClass(prefix.Addr()); hard {
			return HTTPPolicy{}, invalid
		}
		contained := false
		for _, allowed := range append(append([]netip.Prefix(nil), privateHTTPPrefixes...), reservedHTTPPrefixes...) {
			if allowed.Addr().BitLen() == prefix.Addr().BitLen() && prefix.Bits() >= allowed.Bits() && allowed.Contains(prefix.Addr()) {
				contained = true
			}
		}
		if !contained {
			return HTTPPolicy{}, invalid
		}
		policy.prefixes = append(policy.prefixes, prefix)
	}
	return policy, nil
}

func validHTTPPolicyYAML(n *yaml.Node, depth int, budget *int) bool {
	*budget -= 1
	if depth > 16 || *budget < 0 || n.Kind == yaml.AliasNode || (n.Kind == yaml.ScalarNode && n.Tag != "!!str") {
		return false
	}
	if n.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
				return false
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if !validHTTPPolicyYAML(child, depth+1, budget) {
			return false
		}
	}
	return true
}

func (p HTTPPolicy) resolveURL(ctx context.Context, raw string) (*url.URL, netip.Addr, error) {
	if p.loadErr != nil {
		return nil, netip.Addr{}, p.loadErr
	}
	refused := func(class string) (*url.URL, netip.Addr, error) {
		return nil, netip.Addr{}, fmt.Errorf("destination not allowed (%s; check %s)", class, HTTPPolicyFile)
	}
	if strings.IndexFunc(raw, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 {
		return refused("invalid URL")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" || u.User != nil {
		return refused("invalid URL")
	}
	host, err := canonicalHTTPHost(u.Hostname())
	if err != nil {
		return refused("invalid host")
	}
	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 {
			return refused("invalid port")
		}
	}
	var addresses []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{ip}
	} else {
		resolve := p.resolve
		if resolve == nil {
			resolve = func(ctx context.Context, name string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", name)
			}
		}
		addresses, err = resolve(ctx, host)
		if err != nil || len(addresses) == 0 || len(addresses) > 64 {
			return refused("name resolution failed")
		}
	}
	for _, address := range addresses {
		class, hard := httpAddressClass(address)
		allowed := class == ""
		if !hard && class != "" {
			allowed = p.hosts[host]
			for _, prefix := range p.prefixes {
				allowed = allowed || prefix.Contains(normalizeHTTPAddress(address))
			}
		}
		if hard || !allowed {
			slog.Debug("workflow HTTP destination refused", "host", host, "class", class)
			return refused(class)
		}
	}
	return u, normalizeHTTPAddress(addresses[0]), nil
}

func workflowHTTPTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = 30
	}
	if seconds > 120 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}

func (p HTTPPolicy) clientFor(u *url.URL, ip netip.Addr, timeout time.Duration) *http.Client {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	dial := p.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: timeout}).DialContext
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dial(ctx, network, net.JoinHostPort(ip.String(), port))
		},
		TLSClientConfig:       &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
