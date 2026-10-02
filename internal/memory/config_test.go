package memory

import (
	"os"
	"testing"
)

func TestSaveLoadRoundTrip_UserOnlyPerms(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		APIKey:     "act_abc123",
		APIBaseURL: "https://aceteam.ai",
		OrgID:      "org_1",
		OrgName:    "Acme",
		Scopes:     []string{"memory:read", "memory:write"},
	}
	if err := Save(dir, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %o", info.Mode().Perm())
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.APIKey != "act_abc123" || got.OrgName != "Acme" || len(got.Scopes) != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil config for missing file, got %+v", got)
	}
}

func TestEffectiveMCPURL(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{APIBaseURL: "https://aceteam.ai"}, "https://aceteam.ai/api/mcp/aceteam/mcp"},
		{Config{APIBaseURL: "https://aceteam.ai/"}, "https://aceteam.ai/api/mcp/aceteam/mcp"},
		{Config{}, "https://aceteam.ai/api/mcp/aceteam/mcp"},
		{Config{MCPURL: "https://custom/mcp"}, "https://custom/mcp"},
	}
	for _, c := range cases {
		if got := c.cfg.EffectiveMCPURL(); got != c.want {
			t.Errorf("EffectiveMCPURL(%+v)=%q want %q", c.cfg, got, c.want)
		}
	}
}
