package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantKind SourceKind
		wantURL  string // CloneURL (KindGitHub/KindGitURL)
		wantRef  string
		wantName string // KindCatalog
		wantErr  bool
	}{
		{
			name:     "plain catalog name",
			input:    "vllm",
			wantKind: KindCatalog,
			wantName: "vllm",
		},
		{
			name:     "catalog name with dash",
			input:    "whatsapp-bridge",
			wantKind: KindCatalog,
			wantName: "whatsapp-bridge",
		},
		{
			name:     "owner/repo shorthand",
			input:    "sunapi386/whatsapp-bridge",
			wantKind: KindGitHub,
			wantURL:  "https://github.com/sunapi386/whatsapp-bridge.git",
		},
		{
			name:     "owner/repo with ref",
			input:    "sunapi386/whatsapp-bridge@v1.2.0",
			wantKind: KindGitHub,
			wantURL:  "https://github.com/sunapi386/whatsapp-bridge.git",
			wantRef:  "v1.2.0",
		},
		{
			name:     "owner/repo with .git suffix",
			input:    "owner/repo.git",
			wantKind: KindGitHub,
			wantURL:  "https://github.com/owner/repo.git",
		},
		{
			name:     "https url",
			input:    "https://github.com/owner/repo.git",
			wantKind: KindGitURL,
			wantURL:  "https://github.com/owner/repo.git",
		},
		{
			name:     "https url with #ref",
			input:    "https://github.com/owner/repo.git#main",
			wantKind: KindGitURL,
			wantURL:  "https://github.com/owner/repo.git",
			wantRef:  "main",
		},
		{
			name:     "https url with @ref after path",
			input:    "https://github.com/owner/repo@v2.0.0",
			wantKind: KindGitURL,
			wantURL:  "https://github.com/owner/repo",
			wantRef:  "v2.0.0",
		},
		{
			name:     "https url with userinfo is preserved (no ref)",
			input:    "https://user@github.com/owner/repo.git",
			wantKind: KindGitURL,
			wantURL:  "https://user@github.com/owner/repo.git",
			wantRef:  "",
		},
		{
			name:     "scp-form git remote (ref untouched, @ preserved)",
			input:    "git@github.com:owner/repo.git",
			wantKind: KindGitURL,
			wantURL:  "git@github.com:owner/repo.git",
			wantRef:  "",
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			input:   "   ",
			wantErr: true,
		},
		{
			name:    "trailing slash invalid owner/repo",
			input:   "owner/",
			wantErr: true,
		},
		{
			name:    "leading slash invalid owner/repo",
			input:   "/repo",
			wantErr: true,
		},
		{
			name:    "too many slashes",
			input:   "a/b/c",
			wantErr: true,
		},
		{
			name:    "catalog name with colon invalid",
			input:   "weird:name",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSource(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSource(%q) expected error, got %+v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSource(%q) unexpected error: %v", tt.input, err)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tt.wantKind)
			}
			if tt.wantURL != "" && got.CloneURL != tt.wantURL {
				t.Errorf("CloneURL = %q, want %q", got.CloneURL, tt.wantURL)
			}
			if got.Ref != tt.wantRef {
				t.Errorf("Ref = %q, want %q", got.Ref, tt.wantRef)
			}
			if tt.wantName != "" && got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
		})
	}
}

func TestParseSourceGitHubOwnerRepo(t *testing.T) {
	got, err := ParseSource("acme/my-module@release-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Owner != "acme" || got.Repo != "my-module" {
		t.Errorf("Owner/Repo = %q/%q, want acme/my-module", got.Owner, got.Repo)
	}
	if got.Ref != "release-1" {
		t.Errorf("Ref = %q, want release-1", got.Ref)
	}
}

// writeFile is a tiny test helper that creates parent dirs and writes content.
func writeModuleFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const sampleManifest = "name: my-module\nversion: 1.0.0\ndescription: test\n"
const sampleCompose = "services:\n  app:\n    image: ghcr.io/acme/my-module:latest\n"

func TestLoadModuleManifest_CitadelSubdir(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, filepath.Join(dir, "citadel", "service.yaml"), sampleManifest)
	writeModuleFile(t, filepath.Join(dir, "citadel", "compose.yml"), sampleCompose)

	m, composePath, err := loadModuleManifest(dir)
	if err != nil {
		t.Fatalf("loadModuleManifest: %v", err)
	}
	if m.Name != "my-module" {
		t.Errorf("Name = %q, want my-module", m.Name)
	}
	if composePath != filepath.Join(dir, "citadel", "compose.yml") {
		t.Errorf("composePath = %q, want citadel/compose.yml", composePath)
	}
}

func TestLoadModuleManifest_RepoRootFallback(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, filepath.Join(dir, "service.yaml"), sampleManifest)
	writeModuleFile(t, filepath.Join(dir, "compose.yml"), sampleCompose)

	m, composePath, err := loadModuleManifest(dir)
	if err != nil {
		t.Fatalf("loadModuleManifest: %v", err)
	}
	if m.Name != "my-module" {
		t.Errorf("Name = %q, want my-module", m.Name)
	}
	if composePath != filepath.Join(dir, "compose.yml") {
		t.Errorf("composePath = %q, want repo-root compose.yml", composePath)
	}
}

func TestLoadModuleManifest_CitadelPreferredOverRoot(t *testing.T) {
	dir := t.TempDir()
	// Both present: citadel/ should win.
	writeModuleFile(t, filepath.Join(dir, "citadel", "service.yaml"), "name: from-citadel\nversion: 1\n")
	writeModuleFile(t, filepath.Join(dir, "citadel", "compose.yml"), sampleCompose)
	writeModuleFile(t, filepath.Join(dir, "service.yaml"), "name: from-root\nversion: 1\n")
	writeModuleFile(t, filepath.Join(dir, "compose.yml"), sampleCompose)

	m, composePath, err := loadModuleManifest(dir)
	if err != nil {
		t.Fatalf("loadModuleManifest: %v", err)
	}
	if m.Name != "from-citadel" {
		t.Errorf("Name = %q, want from-citadel (citadel/ subdir preferred)", m.Name)
	}
	if composePath != filepath.Join(dir, "citadel", "compose.yml") {
		t.Errorf("composePath = %q, want citadel/compose.yml", composePath)
	}
}

func TestLoadModuleManifest_NoManifest(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, filepath.Join(dir, "README.md"), "nothing here")
	if _, _, err := loadModuleManifest(dir); err == nil {
		t.Fatal("expected error when no service.yaml is present")
	}
}

func TestLoadModuleManifest_ManifestButNoCompose(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, filepath.Join(dir, "citadel", "service.yaml"), sampleManifest)
	if _, _, err := loadModuleManifest(dir); err == nil {
		t.Fatal("expected error when compose.yml is missing")
	}
}
