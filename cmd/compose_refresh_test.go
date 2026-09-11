package cmd

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// loopbackComposeContent is a minimal materialized-template stand-in that
// publishes its host port on 127.0.0.1 (the #1025 loopback shape).
const loopbackComposeContent = `services:
  vllm:
    image: example
    ports:
      - "127.0.0.1:${CITADEL_VLLM_HOST_PORT}:8000"
`

// handEditedWildcardComposeContent is a materialized template an operator
// hand-edited to publish on all interfaces. composerefresh.Sweep preserves such
// a file (its hash matches neither the stamp nor KnownComposeHashes), so the
// drift remediation must NOT recreate against it -- otherwise it would restart
// every boot forever.
const handEditedWildcardComposeContent = `services:
  vllm:
    image: example
    ports:
      - "0.0.0.0:8201:8000"
`

func TestParseHostBindings(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []hostBinding
	}{
		{
			name: "loopback single binding",
			raw:  "127.0.0.1|8201 ",
			want: []hostBinding{{HostIP: "127.0.0.1", HostPort: 8201}},
		},
		{
			name: "docker wildcard emits v4 and v6",
			raw:  "0.0.0.0|8201 ::|8201 ",
			want: []hostBinding{{HostIP: "0.0.0.0", HostPort: 8201}, {HostIP: "::", HostPort: 8201}},
		},
		{
			name: "podman wildcard is empty HostIp",
			raw:  "|8201 ",
			want: []hostBinding{{HostIP: "", HostPort: 8201}},
		},
		{
			name: "ipv6 loopback keeps its colons",
			raw:  "::1|8201 ",
			want: []hostBinding{{HostIP: "::1", HostPort: 8201}},
		},
		{
			name: "skips zero and unparseable ports",
			raw:  "127.0.0.1|0 127.0.0.1|abc 127.0.0.1|8201",
			want: []hostBinding{{HostIP: "127.0.0.1", HostPort: 8201}},
		},
		{
			name: "empty output",
			raw:  "   ",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHostBindings(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("parseHostBindings(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseHostBindings(%q)[%d] = %+v, want %+v", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsWildcardHostIP(t *testing.T) {
	wildcard := []string{"", "0.0.0.0", "::", "  0.0.0.0  "}
	for _, ip := range wildcard {
		if !isWildcardHostIP(ip) {
			t.Errorf("isWildcardHostIP(%q) = false, want true", ip)
		}
	}
	notWildcard := []string{"127.0.0.1", "::1", "192.168.1.5", "10.0.0.2"}
	for _, ip := range notWildcard {
		if isWildcardHostIP(ip) {
			t.Errorf("isWildcardHostIP(%q) = true, want false", ip)
		}
	}
}

func TestComposePublishesLoopbackOnly(t *testing.T) {
	if !composePublishesLoopbackOnly(loopbackComposeContent) {
		t.Error("loopback template must be reported loopback-only")
	}
	if composePublishesLoopbackOnly(handEditedWildcardComposeContent) {
		t.Error("hand-edited 0.0.0.0 template must NOT be reported loopback-only")
	}
	// No host publish at all (container-port-only) is not loopback-only: there
	// is nothing to remediate, so the drift check must not fire.
	if composePublishesLoopbackOnly("services:\n  x:\n    ports:\n      - \"8000\"\n") {
		t.Error("container-port-only compose has no host publish; must not report loopback-only")
	}
	// Every real #1025 engine template must read as loopback-only.
	for name := range loopbackDriftEngines {
		if !composePublishesLoopbackOnly(services.ServiceMap[name]) {
			t.Errorf("services.ServiceMap[%q] must publish loopback-only", name)
		}
	}
}

func TestEngineBindDriftRequiresRecreate(t *testing.T) {
	tests := []struct {
		name     string
		service  string
		content  string
		bindings []hostBinding
		want     bool
	}{
		{
			name:     "vllm on 0.0.0.0 with loopback template recreates",
			service:  "vllm",
			content:  services.ServiceMap["vllm"],
			bindings: []hostBinding{{HostIP: "0.0.0.0", HostPort: 8201}, {HostIP: "::", HostPort: 8201}},
			want:     true,
		},
		{
			name:     "podman empty HostIp recreates",
			service:  "sglang",
			content:  services.ServiceMap["sglang"],
			bindings: []hostBinding{{HostIP: "", HostPort: 30000}},
			want:     true,
		},
		{
			name:     "vllm already on loopback is left alone",
			service:  "vllm",
			content:  services.ServiceMap["vllm"],
			bindings: []hostBinding{{HostIP: "127.0.0.1", HostPort: 8201}},
			want:     false,
		},
		{
			name:     "ipv6 loopback is left alone",
			service:  "bonsai",
			content:  services.ServiceMap["bonsai"],
			bindings: []hostBinding{{HostIP: "::1", HostPort: 8210}},
			want:     false,
		},
		{
			name:     "explicit LAN IP is not wildcard, left alone",
			service:  "llamacpp",
			content:  services.ServiceMap["llamacpp"],
			bindings: []hostBinding{{HostIP: "192.168.1.5", HostPort: 8211}},
			want:     false,
		},
		{
			name:     "loop guard: hand-edited 0.0.0.0 file is not recreated",
			service:  "vllm",
			content:  handEditedWildcardComposeContent,
			bindings: []hostBinding{{HostIP: "0.0.0.0", HostPort: 8201}},
			want:     false,
		},
		{
			name:     "ServiceMap engine outside the #1025 set is left alone",
			service:  "extraction",
			content:  services.ServiceMap["extraction"],
			bindings: []hostBinding{{HostIP: "0.0.0.0", HostPort: 8100}},
			want:     false,
		},
		{
			name:     "third-party/non-ServiceMap service is untouched",
			service:  "whatsapp-bridge",
			content:  loopbackComposeContent,
			bindings: []hostBinding{{HostIP: "0.0.0.0", HostPort: 8201}},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := engineBindDriftRequiresRecreate(tt.service, tt.content, tt.bindings)
			if got != tt.want {
				t.Errorf("engineBindDriftRequiresRecreate(%q, ...) = %v, want %v", tt.service, got, tt.want)
			}
		})
	}
}

// TestLoopbackDriftEnginesPublishLoopback pins the loopbackDriftEngines set to
// the actual embedded templates: every named engine must exist in
// services.ServiceMap AND publish loopback-only, so the set cannot silently
// disagree with what aceteam-ai/aceteam#9523 (PR #1025) edited.
func TestLoopbackDriftEnginesPublishLoopback(t *testing.T) {
	for name := range loopbackDriftEngines {
		content, ok := services.ServiceMap[name]
		if !ok {
			t.Errorf("loopbackDriftEngines[%q] is not a services.ServiceMap entry", name)
			continue
		}
		if !composePublishesLoopbackOnly(content) {
			t.Errorf("loopbackDriftEngines[%q] template does not publish loopback-only", name)
		}
	}
}
