package engine

import (
	"testing"

	"github.com/aceteam-ai/citadel-cli/services"
)

// TestFirstComposeHostPort is the citadel #685 slice 2 relocation of
// internal/status's identically-named test (internal/status/engines_test.go)
// -- firstComposeHostPort moved here along with the function it tests.
func TestFirstComposeHostPort(t *testing.T) {
	cases := []struct {
		name    string
		compose string
		want    int
	}{
		{
			name: "simple mapping",
			compose: `services:
  vllm:
    image: vllm/vllm-openai:latest
    ports:
      - "8100:8000"
`,
			want: 8100,
		},
		{
			name: "ip-qualified mapping",
			compose: `services:
  svc:
    ports:
      - "127.0.0.1:9000:8000"
`,
			want: 9000,
		},
		{
			name: "no ports",
			compose: `services:
  svc:
    image: foo
`,
			want: 0,
		},
		{
			name:    "malformed yaml",
			compose: "::: not yaml",
			want:    0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstComposeHostPort(c.compose); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

// TestHostPortForName_VLLM mirrors internal/status's
// TestManagedEngineHostPort_VLLM (now a thin wrapper over this function).
func TestHostPortForName_VLLM(t *testing.T) {
	if got := HostPortForName("vllm"); got != services.VLLMHostPort {
		t.Fatalf("expected vllm host port %d from registry, got %d", services.VLLMHostPort, got)
	}
	if got := HostPortForName("does-not-exist"); got != 0 {
		t.Fatalf("expected 0 for unknown engine, got %d", got)
	}
}

// TestHostPortForName_NonServiceMapEntry pins the exact gap a naive
// Registry.Lookup-based resolver would reintroduce: a name present in
// services.ManagedServiceHostPort (services.ServiceHostPorts) but absent from
// services.ServiceMap -- e.g. "gotenberg" -- must still resolve, because
// status.collectRunningEmbeddedServices calls this for every running
// "citadel-<name>" container, not just services.ServiceMap engines.
func TestHostPortForName_NonServiceMapEntry(t *testing.T) {
	if _, inServiceMap := services.ServiceMap["gotenberg"]; inServiceMap {
		t.Fatal("test assumption broken: gotenberg is now in services.ServiceMap")
	}
	wantPort, ok := services.ManagedServiceHostPort("gotenberg")
	if !ok {
		t.Fatal("test assumption broken: gotenberg has no services.ManagedServiceHostPort entry")
	}
	if got := HostPortForName("gotenberg"); got != wantPort {
		t.Fatalf("HostPortForName(%q) = %d, want %d (services.ManagedServiceHostPort, independent of services.ServiceMap membership)", "gotenberg", got, wantPort)
	}
}

// TestTypeFromName pins the substring-matching contract this function
// replaced two independently-maintained duplicates with (internal/status's
// original EngineTypeFromName and internal/mesh's copy of it, citadel #685
// §1c). Also asserts today's patterns never collide (i.e. iteration order is
// not load-bearing), per this function's own doc comment.
func TestTypeFromName(t *testing.T) {
	cases := map[string]string{
		"vllm":             "vllm",
		"citadel-vllm":     "vllm",
		"ollama":           "ollama",
		"OLLAMA-Big":       "ollama", // case-insensitive
		"bonsai":           "bonsai",
		"unlimited-ocr":    "unlimited-ocr",
		"llamacpp":         "llamacpp",
		"llama.cpp":        "llamacpp",
		"llama-cpp-server": "llamacpp",
		"sglang":           "sglang",
		"citadel-sglang":   "sglang",
		"postgres":         "",
		"tei":              "", // not a managed-probe engine
	}
	for in, want := range cases {
		if got := TypeFromName(in); got != want {
			t.Errorf("TypeFromName(%q) = %q, want %q", in, got, want)
		}
	}
}
