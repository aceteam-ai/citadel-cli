package workflow

import (
	"bytes"
	"context"
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

const LLMPolicyFile = "workflow-llm-policy.yaml"

const (
	maxLLMPolicyBytes   = 16 << 10
	maxLLMEndpoints     = 64
	maxLLMURLBytes      = 4096
	llmRequestTimeout   = 5 * time.Minute
	maxLLMResponseBytes = 10 << 20
	defaultLLMEndpoint  = "http://127.0.0.1:11434/api/chat"
)

// ErrLLMResponseTooLarge means no response/result was accepted.
var ErrLLMResponseTooLarge = errors.New("LLM response exceeds the size limit")

var llmMeshPrefix = netip.MustParsePrefix("100.64.0.0/10")

// LLMPolicy is immutable at construction. Its zero value permits public URLs
// and the exact built-in Ollama endpoint. Other loopback/private endpoints need
// machine-local exact grants; graphs and HTTPPolicy cannot supply them.
type LLMPolicy struct {
	endpoints map[string]bool
	loadErr   error
	loadWhy   string // bounded local diagnostic, never raw filesystem/YAML text
	resolve   func(context.Context, string) ([]netip.Addr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
}

// LoadLLMPolicy reads only the explicitly supplied node configuration directory.
// Missing means the default policy. Other errors disable all LLM destinations
// with one content-free diagnostic, never an unrestricted-client fallback.
func LoadLLMPolicy(nodeConfigDir string) LLMPolicy {
	refuse := func(reason string) LLMPolicy {
		slog.Warn("workflow LLM policy unreadable or invalid; LLM destinations disabled", "reason", reason)
		return LLMPolicy{loadErr: errors.New("LLM policy unreadable or invalid"), loadWhy: reason}
	}
	if nodeConfigDir == "" {
		return refuse("node_directory_missing")
	}
	root, err := os.OpenRoot(nodeConfigDir)
	if errors.Is(err, os.ErrNotExist) {
		return LLMPolicy{}
	}
	if err != nil {
		return refuse(httpPolicyIOReason("directory", err))
	}
	defer root.Close()
	before, err := root.Lstat(LLMPolicyFile)
	if errors.Is(err, os.ErrNotExist) {
		return LLMPolicy{}
	}
	if err != nil {
		return refuse(httpPolicyIOReason("stat", err))
	}
	if !before.Mode().IsRegular() {
		return refuse("file_not_regular")
	}
	if before.Size() > maxLLMPolicyBytes {
		return refuse("file_too_large")
	}
	f, err := root.Open(LLMPolicyFile)
	if err != nil {
		return refuse(httpPolicyIOReason("open", err))
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return refuse("file_changed_during_open")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLLMPolicyBytes+1))
	if err != nil {
		return refuse(httpPolicyIOReason("read", err))
	}
	if len(data) > maxLLMPolicyBytes {
		return refuse("file_too_large")
	}
	policy, err := parseLLMPolicy(data)
	if err != nil {
		return refuse("invalid_policy_document")
	}
	return policy
}

func parseLLMPolicy(data []byte) (LLMPolicy, error) {
	invalid := errors.New("invalid LLM policy")
	if len(data) > maxLLMPolicyBytes {
		return LLMPolicy{}, invalid
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&document); err != nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return LLMPolicy{}, invalid
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode || len(root.Content) != 2 || root.Content[0].Value != "allow" || root.Content[1].Kind != yaml.SequenceNode {
		return LLMPolicy{}, invalid
	}
	budget := 4096
	if !validHTTPPolicyYAML(&document, 0, &budget) {
		return LLMPolicy{}, invalid
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		return LLMPolicy{}, invalid
	}
	var config struct {
		Allow []struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"allow"`
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(&config); err != nil || len(config.Allow) > maxLLMEndpoints {
		return LLMPolicy{}, invalid
	}
	policy := LLMPolicy{endpoints: make(map[string]bool)}
	for _, entry := range config.Allow {
		u, err := canonicalLLMEndpoint(entry.Endpoint)
		if err != nil || policy.endpoints[u.String()] {
			return LLMPolicy{}, invalid
		}
		policy.endpoints[u.String()] = true
	}
	return policy, nil
}

// canonicalLLMEndpoint keeps escaped paths distinct and normalizes only host,
// effective port and an empty root path. There is no prefix/origin-only match.
func canonicalLLMEndpoint(raw string) (*url.URL, error) {
	invalid := errors.New("invalid LLM endpoint")
	if len(raw) > maxLLMURLBytes || strings.IndexFunc(raw, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 {
		return nil, invalid
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || strings.HasSuffix(u.Host, ":") {
		return nil, invalid
	}
	host, err := canonicalHTTPHost(u.Hostname())
	if err != nil || (strings.Contains(u.Hostname(), ":") && !strings.HasPrefix(u.Host, "[")) {
		return nil, invalid
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return nil, invalid
	}
	u.Host = net.JoinHostPort(host, strconv.FormatUint(number, 10))
	if u.Path == "" {
		u.Path = "/"
	}
	return u, nil
}

func (p LLMPolicy) resolveURL(ctx context.Context, raw string) (*url.URL, netip.Addr, error) {
	if p.loadErr != nil {
		return nil, netip.Addr{}, p.loadErr
	}
	refused := func(class string) (*url.URL, netip.Addr, error) {
		return nil, netip.Addr{}, fmt.Errorf("LLM destination not allowed (%s; check %s)", class, LLMPolicyFile)
	}
	u, err := canonicalLLMEndpoint(raw)
	if err != nil {
		return refused("invalid endpoint")
	}
	if err := ctx.Err(); err != nil {
		return nil, netip.Addr{}, err
	}
	host := u.Hostname()
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
			if err := ctx.Err(); err != nil {
				return nil, netip.Addr{}, err
			}
			return refused("name resolution failed")
		}
	}
	// This exception is intentionally the exact built-in spelling, not any DNS
	// alias, normalized address spelling, neighboring path or another port.
	granted := raw == defaultLLMEndpoint || p.endpoints[u.String()]
	for _, address := range addresses {
		class, _ := httpAddressClass(address)
		normalized := normalizeHTTPAddress(address)
		private := normalized.IsPrivate() || llmMeshPrefix.Contains(normalized)
		if class != "" && !(granted && (class == "loopback address" || (class == "private address" && private))) {
			return refused(class)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, netip.Addr{}, err
	}
	return u, normalizeHTTPAddress(addresses[0]), nil
}

func (p LLMPolicy) clientFor(u *url.URL, ip netip.Addr) *http.Client {
	return (HTTPPolicy{dial: p.dial}).clientFor(u, ip, llmRequestTimeout)
}

// llmRequestURL leaves canonical grant identity intact while avoiding an
// explicit default port in the wire Host authority (including bracketed IPv6).
func llmRequestURL(u *url.URL) *url.URL {
	request := *u
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		request.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	return &request
}
