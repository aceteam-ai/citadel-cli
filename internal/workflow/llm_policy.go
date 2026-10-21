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
)

// ErrLLMResponseTooLarge means no response/result was accepted.
var ErrLLMResponseTooLarge = errors.New("LLM response exceeds the size limit")

var llmMeshPrefix = netip.MustParsePrefix("100.64.0.0/10")

// LLMPolicy is immutable at construction. Its zero value is public-only. Only
// machine-local exact endpoints can grant loopback/private access; neither the
// workflow graph nor HTTPPolicy grants are authority for an LLM request.
type LLMPolicy struct {
	endpoints map[string]bool
	loadErr   error
	resolve   func(context.Context, string) ([]netip.Addr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
}

// LoadLLMPolicy reads only the explicitly supplied node configuration directory.
// Missing means public-only. Other errors disable LLM destinations, with one
// content-free diagnostic, never a fallback to an unrestricted client.
func LoadLLMPolicy(nodeConfigDir string) LLMPolicy {
	refuse := func() LLMPolicy {
		slog.Warn("workflow LLM policy unreadable or invalid; LLM destinations disabled")
		return LLMPolicy{loadErr: errors.New("LLM policy unreadable or invalid")}
	}
	if nodeConfigDir == "" {
		return refuse()
	}
	root, err := os.OpenRoot(nodeConfigDir)
	if errors.Is(err, os.ErrNotExist) {
		return LLMPolicy{}
	}
	if err != nil {
		return refuse()
	}
	defer root.Close()
	before, err := root.Lstat(LLMPolicyFile)
	if errors.Is(err, os.ErrNotExist) {
		return LLMPolicy{}
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxLLMPolicyBytes {
		return refuse()
	}
	f, err := root.Open(LLMPolicyFile)
	if err != nil {
		return refuse()
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return refuse()
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLLMPolicyBytes+1))
	if err != nil || len(data) > maxLLMPolicyBytes {
		return refuse()
	}
	policy, err := parseLLMPolicy(data)
	if err != nil {
		return refuse()
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
	granted := p.endpoints[u.String()]
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
