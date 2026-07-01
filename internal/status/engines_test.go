package status

import "testing"

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

// TestManagedEngineHostPort_VLLM confirms the embedded vLLM compose resolves to
// its published host port. This guards against the compose file changing its
// port mapping without the idle scraper following (it scrapes the host port).
func TestManagedEngineHostPort_VLLM(t *testing.T) {
	if got := managedEngineHostPort("vllm"); got != 8100 {
		t.Fatalf("expected vllm host port 8100 from embedded compose, got %d", got)
	}
	if got := managedEngineHostPort("does-not-exist"); got != 0 {
		t.Fatalf("expected 0 for unknown engine, got %d", got)
	}
}

func TestIdleEngineType_MatchesImage(t *testing.T) {
	// A catalog slug that doesn't contain "vllm" in the name but whose image does.
	if got := idleEngineType("llm-server vllm/vllm-openai:latest"); got != "vllm" {
		t.Fatalf("expected vllm engine from image hint, got %q", got)
	}
	if got := idleEngineType("postgres postgres:16"); got != "" {
		t.Fatalf("expected empty engine for non-inference hint, got %q", got)
	}
}
