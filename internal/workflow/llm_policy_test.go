package workflow

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustLLMPolicy(t *testing.T, endpoint string) LLMPolicy {
	t.Helper()
	p, err := parseLLMPolicy([]byte("allow:\n  - endpoint: " + endpoint + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLLMPolicyClosedSchema(t *testing.T) {
	for _, data := range []string{
		"", "null", "allow:", "allow: true", "allow: {}", "ALLOW: []",
		"allow: []\nextra: true", "allow: []\nallow: []", "allow: []\n---\nallow: []",
		"allow: [{host: localhost}]", "allow: [{cidr: 127.0.0.0/8}]",
		"allow: [{endpoint: null}]", "allow: [{endpoint: 123}]", "allow: [{}]",
		"allow: [{endpoint: 'http://localhost/api/chat', extra: x}]",
		"allow: [{endpoint: 'http://localhost/api/chat', endpoint: 'http://localhost/other'}]",
		"allow: [ &entry {endpoint: 'http://localhost/api/chat'}, *entry ]",
		"allow: [{endpoint: 'http://localhost/api/chat'}, {endpoint: 'http://LOCALHOST:80/api/chat'}]",
		"allow: []\n" + strings.Repeat(" ", maxLLMPolicyBytes),
	} {
		if _, err := parseLLMPolicy([]byte(data)); err == nil {
			t.Fatalf("invalid policy accepted: %q", data[:min(len(data), 100)])
		}
	}
	for _, count := range []int{0, maxLLMEndpoints, maxLLMEndpoints + 1} {
		var data strings.Builder
		if count == 0 {
			data.WriteString("allow: []\n")
		} else {
			data.WriteString("allow:\n")
		}
		for i := 0; i < count; i++ {
			fmt.Fprintf(&data, "  - endpoint: http://127.0.0.1:11434/api/chat/%d\n", i)
		}
		p, err := parseLLMPolicy([]byte(data.String()))
		if count > maxLLMEndpoints {
			if err == nil {
				t.Fatal("too many endpoint grants accepted")
			}
		} else if err != nil || len(p.endpoints) != count {
			t.Fatalf("bounded policy count=%d: %v", count, err)
		}
	}
}

func TestLLMPolicyInvalidEndpointsDoNotEchoInput(t *testing.T) {
	for _, raw := range []string{
		"", "file:///private-marker", "ftp://public.example/private-marker",
		"http://user:private-marker@public.example/api/chat", "//public.example/private-marker",
		"http://public.example/api/chat?token=private-marker", "http://public.example/api/chat?",
		"http://public.example/api/chat#private-marker", "http://public.example/api/chat#",
		"http://public.example:/private-marker", "http://public.example:0/private-marker",
		"http://public.example:65536/private-marker", "http://public.example:abc/private-marker",
		"http://[fe80::1%25eth0]/private-marker", "http://public.example/private marker",
		"http://2001:4860:4860::8888:80/private-marker",
		"http://public.example/private\nmarker", "http://public.example/%private-marker",
		"http://*.example/private-marker", "http://public.example/" + strings.Repeat("x", maxLLMURLBytes),
	} {
		p := LLMPolicy{resolve: func(context.Context, string) ([]netip.Addr, error) {
			t.Error("invalid endpoint reached name resolution")
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}}
		_, _, err := p.resolveURL(context.Background(), raw)
		if err == nil || strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "public.example") {
			t.Fatalf("invalid endpoint accepted or echoed: %v", err)
		}
	}
}

func TestLLMPolicyDefaultDeniesPrivateAndHardRanges(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1", "127.1.2.3", "::1", "::ffff:127.0.0.1", "::127.0.0.1",
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "fc00::1",
		"169.254.169.254", "fe80::1", "100.100.100.100", "fd7a:115c:a1e0::53",
		"0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "ff02::1", "255.255.255.255",
		"192.0.2.1", "198.18.0.1", "2001:db8::1", "64:ff9b::7f00:1", "64:ff9b:1::a00:1",
		"2002:7f00:1::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2", "::ffff:0:7f00:1",
	} {
		endpoint := "http://" + net.JoinHostPort(address, "11435") + "/api/chat"
		if _, _, err := (LLMPolicy{}).resolveURL(context.Background(), endpoint); err == nil {
			t.Fatalf("default policy allowed %s", address)
		}
	}
}

func TestLLMPolicyBuiltInDefaultIsExact(t *testing.T) {
	p := LLMPolicy{resolve: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}}
	if _, ip, err := p.resolveURL(context.Background(), defaultLLMEndpoint); err != nil || ip.String() != "127.0.0.1" {
		t.Fatalf("built-in Ollama endpoint refused: %v", err)
	}
	for _, raw := range []string{
		"http://localhost:11434/api/chat", "http://127.0.0.2:11434/api/chat",
		"http://127.0.0.1:11435/api/chat", "https://127.0.0.1:11434/api/chat",
		"http://127.0.0.1:11434/api/delete", "http://127.0.0.1:11434/api/pull",
		"http://127.0.0.1:11434/api/chat/", "http://127.0.0.1:11434/api/%63hat",
		"http://127.0.0.1:11434/api/x/../chat", "http://127.0.0.1:11434/api//chat",
		"http://127.0.0.1:011434/api/chat", "http://[::ffff:127.0.0.1]:11434/api/chat",
		"http://[::7f00:1]:11434/api/chat",
	} {
		if _, _, err := p.resolveURL(context.Background(), raw); err == nil {
			t.Fatalf("built-in exception authorized another endpoint: %q", raw)
		}
	}
	if _, _, err := (LLMPolicy{}).resolveURL(context.Background(), "https://8.8.8.8/api/chat"); err != nil {
		t.Fatalf("built-in exception changed public endpoint policy: %v", err)
	}
}

func TestLLMPolicyMeshGrantAndEmptyPathEquivalence(t *testing.T) {
	const mesh = "http://100.64.0.58:11434/api/chat"
	if _, ip, err := mustLLMPolicy(t, mesh).resolveURL(context.Background(), mesh); err != nil || ip.String() != "100.64.0.58" {
		t.Fatalf("explicit mesh endpoint refused: %v", err)
	}
	for _, grant := range []string{"http://127.0.0.1:11434", "http://127.0.0.1:11434/"} {
		p := mustLLMPolicy(t, grant)
		for _, endpoint := range []string{"http://127.0.0.1:11434", "http://127.0.0.1:11434/"} {
			if u, _, err := p.resolveURL(context.Background(), endpoint); err != nil || u.Path != "/" {
				t.Fatalf("empty/root path equivalence refused: %v", err)
			}
		}
	}
}

func TestLLMRequestURLDefaultHostPort(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		host     string
	}{
		{"http://model.example/api/chat", "model.example"},
		{"http://model.example:80/api/chat", "model.example"},
		{"https://model.example:443/api/chat", "model.example"},
		{"http://[2606:4700:4700::1111]:80/api/chat", "[2606:4700:4700::1111]"},
		{"https://[2606:4700:4700::1111]:443/api/chat", "[2606:4700:4700::1111]"},
		{defaultLLMEndpoint, "127.0.0.1:11434"},
	} {
		u, err := canonicalLLMEndpoint(tc.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		canonical := u.String()
		req, err := http.NewRequest(http.MethodPost, llmRequestURL(u).String(), nil)
		if err != nil || req.Host != tc.host || u.String() != canonical {
			t.Fatalf("request Host or canonical grant changed for %q: %v", tc.endpoint, err)
		}
	}
}

func TestLLMPolicyExactGrantCannotAuthorizeOtherEndpointsOrHardRanges(t *testing.T) {
	const endpoint = "http://127.0.0.1:11434/api/chat"
	p := mustLLMPolicy(t, endpoint)
	if _, ip, err := p.resolveURL(context.Background(), endpoint); err != nil || ip.String() != "127.0.0.1" {
		t.Fatalf("explicit local Ollama grant refused: %v", err)
	}
	for _, raw := range []string{
		"http://127.0.0.1:11435/api/chat", "https://127.0.0.1:11434/api/chat",
		"http://127.0.0.2:11434/api/chat", "http://127.0.0.1:11434/api/delete",
		"http://127.0.0.1:11434/api/chat/", "http://127.0.0.1:11434/api/%63hat",
		"http://127.0.0.1:11434/api/x/../chat", "http://127.0.0.1:11434/api//chat",
	} {
		if _, _, err := p.resolveURL(context.Background(), raw); err == nil {
			t.Fatalf("exact grant authorized a different endpoint: %q", raw)
		}
	}
	for _, address := range []string{
		"169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1",
		"100.100.100.100", "fd7a:115c:a1e0::53", "192.0.2.1", "198.18.0.1", "2001:db8::1",
		"64:ff9b::7f00:1", "2002:7f00:1::1", "::ffff:0:7f00:1",
	} {
		raw := "http://" + net.JoinHostPort(address, "11434") + "/api/chat"
		if _, _, err := mustLLMPolicy(t, raw).resolveURL(context.Background(), raw); err == nil {
			t.Fatalf("exact grant bypassed a hard-denied range: %s", address)
		}
	}
	private := "http://192.168.1.2:8000/api/chat"
	if _, _, err := mustLLMPolicy(t, private).resolveURL(context.Background(), private); err != nil {
		t.Fatalf("explicit private endpoint refused: %v", err)
	}
	// An LLM grant is not an exception for the general HTTP node.
	if _, _, err := (HTTPPolicy{}).resolveURL(context.Background(), endpoint); err == nil {
		t.Fatal("LLM policy weakened HTTP loopback denial")
	}
}

func TestLLMPolicyDNSChecksEveryAnswer(t *testing.T) {
	const endpoint = "http://model.example:11434/api/chat"
	for _, answers := range [][]netip.Addr{
		{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("8.8.8.8")},
		{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")},
		{netip.MustParseAddr("8.8.8.8"), {}},
	} {
		p := LLMPolicy{resolve: func(context.Context, string) ([]netip.Addr, error) { return answers, nil }}
		if _, _, err := p.resolveURL(context.Background(), endpoint); err == nil {
			t.Fatal("mixed denied DNS answer was ignored")
		}
	}
	p := mustLLMPolicy(t, endpoint)
	p.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("169.254.169.254")}, nil
	}
	if _, _, err := p.resolveURL(context.Background(), endpoint); err == nil {
		t.Fatal("endpoint grant overrode denied DNS metadata answer")
	}
	for _, count := range []int{0, 65} {
		p.resolve = func(context.Context, string) ([]netip.Addr, error) {
			addresses := make([]netip.Addr, count)
			for i := range addresses {
				addresses[i] = netip.MustParseAddr("8.8.8.8")
			}
			return addresses, nil
		}
		if _, _, err := p.resolveURL(context.Background(), endpoint); err == nil {
			t.Fatalf("unbounded/empty DNS count accepted: %d", count)
		}
	}
	p.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("private-marker DNS failure")
	}
	if _, _, err := p.resolveURL(context.Background(), endpoint); err == nil || strings.Contains(err.Error(), "private-marker") {
		t.Fatal("DNS error escaped the static boundary")
	}
}

func TestLoadLLMPolicyMissingInvalidAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if p := LoadLLMPolicy(dir); p.loadErr != nil || len(p.endpoints) != 0 {
		t.Fatal("missing policy did not retain defaults")
	}
	if p := LoadLLMPolicy(filepath.Join(dir, "absent-directory")); p.loadErr != nil {
		t.Fatal("missing node directory did not retain defaults")
	}
	if p := LoadLLMPolicy(""); p.loadErr == nil || p.loadWhy != "node_directory_missing" {
		t.Fatal("empty config location accepted or lost its bounded diagnostic")
	}
	path := filepath.Join(dir, LLMPolicyFile)
	for _, tc := range []struct {
		data   string
		reason string
	}{
		{"invalid: private-marker", "invalid_policy_document"},
		{"allow: []\n" + strings.Repeat(" ", maxLLMPolicyBytes), "file_too_large"},
	} {
		if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
			t.Fatal(err)
		}
		p := LoadLLMPolicy(dir)
		if p.loadErr == nil || p.loadWhy != tc.reason || strings.Contains(p.loadErr.Error(), "private-marker") {
			t.Fatal("invalid/oversized policy lost its bounded failure diagnostic")
		}
		if _, _, err := p.resolveURL(context.Background(), "http://8.8.8.8/api/chat"); err == nil {
			t.Fatal("load failure still allowed public traffic")
		}
		if _, _, err := p.resolveURL(context.Background(), defaultLLMEndpoint); err == nil {
			t.Fatal("load failure still allowed built-in Ollama")
		}
	}
	if err := os.WriteFile(path, []byte("allow: [{endpoint: 'http://127.0.0.1:11434/api/chat'}]"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := LoadLLMPolicy(dir)
	if _, _, err := p.resolveURL(context.Background(), "http://127.0.0.1:11434/api/chat"); err != nil {
		t.Fatalf("configured local endpoint not loaded: %v", err)
	}
	aliasDir := t.TempDir()
	if err := os.Symlink(path, filepath.Join(aliasDir, LLMPolicyFile)); err != nil {
		t.Skipf("symlink unavailable on this test host: %v", err)
	}
	if p := LoadLLMPolicy(aliasDir); p.loadErr == nil || p.loadWhy != "file_not_regular" {
		t.Fatal("symlinked policy was accepted or lost its bounded diagnostic")
	}
}
