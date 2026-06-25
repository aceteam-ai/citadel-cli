package catalog

import "testing"

func gh(owner, repo string) Source {
	return Source{Kind: KindGitHub, Owner: owner, Repo: repo, CloneURL: "https://github.com/" + owner + "/" + repo + ".git"}
}

func TestMatchTrust_GitHub(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		src      Source
		want     bool
	}{
		{"exact owner/repo", []string{"acme/widget"}, gh("acme", "widget"), true},
		{"owner wildcard", []string{"acme/*"}, gh("acme", "widget"), true},
		{"owner wildcard other repo", []string{"acme/*"}, gh("acme", "gadget"), true},
		{"host github.com", []string{"github.com"}, gh("anyone", "thing"), true},
		{"no match different owner", []string{"acme/*"}, gh("evil", "thing"), false},
		{"no match different repo", []string{"acme/widget"}, gh("acme", "gadget"), false},
		{"empty patterns", nil, gh("acme", "widget"), false},
		{"blank pattern ignored", []string{"  ", "acme/widget"}, gh("acme", "widget"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchTrust(tt.patterns, tt.src); got != tt.want {
				t.Errorf("matchTrust(%v, %s/%s) = %v, want %v", tt.patterns, tt.src.Owner, tt.src.Repo, got, tt.want)
			}
		})
	}
}

func TestMatchTrust_GitURLHost(t *testing.T) {
	src := Source{Kind: KindGitURL, CloneURL: "https://git.example.com/owner/repo.git"}
	if !matchTrust([]string{"git.example.com"}, src) {
		t.Error("expected host-level trust for git URL")
	}
	if matchTrust([]string{"other.example.com"}, src) {
		t.Error("unexpected match for a different host")
	}
	// owner/repo patterns do not match raw git URLs (known limitation).
	if matchTrust([]string{"owner/repo"}, src) {
		t.Error("owner/repo should not match a raw git URL")
	}

	scp := Source{Kind: KindGitURL, CloneURL: "git@github.com:owner/repo.git"}
	if !matchTrust([]string{"github.com"}, scp) {
		t.Error("expected host-level trust for scp-form git URL")
	}
}

func TestIsTrusted_CatalogAlways(t *testing.T) {
	// Catalog sources are Tier-0, always trusted, no IO needed.
	if !IsTrusted(Source{Kind: KindCatalog, Name: "vllm"}) {
		t.Error("catalog source must always be trusted")
	}
}

func TestTrustedSourcesPersistence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	ts, err := LoadTrustedSources()
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(ts.Patterns) != 0 {
		t.Fatalf("expected empty allowlist, got %v", ts.Patterns)
	}

	if err := AddTrustedSource("acme/*"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := AddTrustedSource("acme/*"); err != nil { // idempotent
		t.Fatalf("add idempotent: %v", err)
	}
	if err := AddTrustedSource("github.com"); err != nil {
		t.Fatalf("add host: %v", err)
	}

	// IsTrusted now reads the persisted file.
	if !IsTrusted(gh("acme", "widget")) {
		t.Error("acme/widget should be trusted via acme/*")
	}

	if err := RemoveTrustedSource("acme/*"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	ts, _ = LoadTrustedSources()
	if len(ts.Patterns) != 1 || ts.Patterns[0] != "github.com" {
		t.Errorf("after remove, expected [github.com], got %v", ts.Patterns)
	}
}
