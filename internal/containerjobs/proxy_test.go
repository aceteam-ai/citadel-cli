package containerjobs

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func proxyOptions(args []string) (map[string]string, []string) {
	env := map[string]string{}
	var hosts []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--env" {
			key, value, _ := strings.Cut(args[i+1], "=")
			env[key] = value
		}
		if args[i] == "--add-host" {
			hosts = append(hosts, args[i+1])
		}
	}
	return env, hosts
}

func TestContainerEgressProxyIsAbsentByDefaultAndIgnoresAmbientOrJobValues(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		t.Setenv(key, "http://ambient.invalid:3128")
	}
	r := baseRequest()
	r.Image = "ghcr.io/aceteam-ai/workspace:v1"
	r.QuotaBytes, r.CPUs, r.MemoryBytes, r.PidsLimit = 16<<20, 1, 1<<30, 128
	r.Network = "aceteam-net-" + strings.TrimPrefix(r.ContainerRef, "sandbox-")
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if json.Unmarshal(b, &payload) != nil {
		t.Fatal("fixture")
	}
	payload["container_egress_proxy_url"] = "http://job-controlled.invalid:3128"
	payload["HTTP_PROXY"] = "http://job-controlled.invalid:3128"
	payload["NO_PROXY"] = "*"
	b, _ = json.Marshal(payload)
	r, err = Parse(Create, b)
	if err != nil {
		t.Fatal(err)
	}
	args, err := createArguments(r, HostPolicy{}, false)
	if err != nil {
		t.Fatal(err)
	}
	env, hosts := proxyOptions(args)
	if len(env) != 0 || len(hosts) != 0 || strings.Contains(strings.Join(args, " "), "proxy") {
		t.Fatal("unconfigured proxy inherited host/job settings or gateway", args)
	}
}

func TestContainerEgressProxyArgumentsUseOnlyExplicitNodePolicy(t *testing.T) {
	for _, proxy := range []string{"http://host.docker.internal:3128", "http://host.docker.internal:3128/"} {
		t.Run(proxy, func(t *testing.T) {
			r := baseRequest()
			r.Image = "ghcr.io/aceteam-ai/workspace:v1"
			r.Network = "aceteam-net-" + strings.TrimPrefix(r.ContainerRef, "sandbox-")
			args, err := createArguments(r, HostPolicy{Runtime: "runsc", EgressProxyURL: proxy}, false)
			if err != nil {
				t.Fatal(err)
			}
			env, hosts := proxyOptions(args)
			want := map[string]string{
				"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "http_proxy": proxy, "https_proxy": proxy,
				"NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1",
			}
			if !reflect.DeepEqual(env, want) || !reflect.DeepEqual(hosts, []string{"host.docker.internal:host-gateway"}) {
				t.Fatal("explicit proxy/gateway contract mismatch", env, hosts)
			}
			if args[len(args)-1] != r.Image || !strings.Contains(strings.Join(args, " "), "--network "+r.Network) {
				t.Fatal("proxy changed container image or network")
			}
		})
	}
}

func TestContainerEgressProxyURLValidationIsFailClosedWithoutReflection(t *testing.T) {
	for _, raw := range []string{
		"http://user:secret@host.docker.internal:3128", "socks5://host.docker.internal:3128", "http://", "host.docker.internal:3128",
		"http://host.docker.internal:0", "http://host.docker.internal:65536", "http://host.docker.internal:", "http://host.docker.internal:bad",
		"http://host.docker.internal:3128/path", "http://host.docker.internal:3128?token=secret", "http://host.docker.internal:3128#secret",
		"http://host.docker.internal:3128?", "http://host.docker.internal:3128\n", "http:// host.docker.internal:3128", strings.Repeat("x", 2049),
		"http://host.docker.internal:3128#", "http://host.docker.internal:3128%2f", "http://host.docker.internal:3128/%2f",
		"http://proxy.example:3128", "http://host.docker.internal:3129", "http://127.0.0.1:3128", "http://[::1]:3128", "http://host.docker.internal.evil:3128",
		"https://host.docker.internal:3128",
	} {
		if ValidEgressProxyURL(raw) {
			t.Fatal("invalid proxy URL accepted")
		}
		args, err := createArguments(baseRequest(), HostPolicy{EgressProxyURL: raw}, false)
		if err != ErrInvalid || len(args) != 0 || strings.Contains(err.Error(), raw) {
			t.Fatal("invalid proxy created arguments or reflected configuration")
		}
	}
	for _, raw := range []string{"", "http://host.docker.internal:3128", "http://host.docker.internal:3128/"} {
		if !ValidEgressProxyURL(raw) {
			t.Fatal("valid explicit node proxy refused")
		}
	}
}
