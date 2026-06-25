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

func TestLooksLikeSHA(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"abc1234", true},  // 7 hex (min)
		{"deadbeef", true}, // 8 hex
		{"0123456789abcdef0123456789abcdef01234567", true}, // 40 hex (full)
		{"ABC1234", true},    // uppercase hex
		{"v1.2.0", false},    // tag
		{"main", false},      // branch
		{"release-1", false}, // branch with dash
		{"abc123", false},    // 6 chars (too short)
		{"abc123g", false},   // non-hex char 'g'
		{"0123456789abcdef0123456789abcdef012345678", false}, // 41 chars (too long)
		{"", false},
	}
	for _, tt := range tests {
		if got := looksLikeSHA(tt.ref); got != tt.want {
			t.Errorf("looksLikeSHA(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}

func TestPickCloneStrategy(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want cloneStrategy
	}{
		{"no ref", "", strategyPlain},
		{"tag", "v1.2.0", strategyBranch},
		{"branch", "main", strategyBranch},
		{"sha", "deadbeef", strategyFetchSHA},
		{"full sha", "0123456789abcdef0123456789abcdef01234567", strategyFetchSHA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickCloneStrategy(Source{Kind: KindGitHub, Ref: tt.ref})
			if got != tt.want {
				t.Errorf("pickCloneStrategy(ref=%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestParseComposeImages(t *testing.T) {
	compose := `services:
  app:
    image: ghcr.io/acme/app:latest
    container_name: acme-app
  db:
    image: "postgres:16"
  cache:
    image: ghcr.io/acme/app:latest
`
	got := parseComposeImages(compose)
	want := []string{"ghcr.io/acme/app:latest", "postgres:16"}
	if len(got) != len(want) {
		t.Fatalf("parseComposeImages = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("image[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseComposeImages_None(t *testing.T) {
	if got := parseComposeImages("services:\n  app:\n    build: .\n"); len(got) != 0 {
		t.Errorf("expected no images, got %v", got)
	}
}

func TestParseComposeContainerName(t *testing.T) {
	compose := "services:\n  app:\n    image: x\n    container_name: \"my-app\"\n"
	if got := parseComposeContainerName(compose); got != "my-app" {
		t.Errorf("parseComposeContainerName = %q, want my-app", got)
	}
	if got := parseComposeContainerName("services:\n  app:\n    image: x\n"); got != "" {
		t.Errorf("expected empty container name, got %q", got)
	}
}

func TestCloneErrorHost(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo.git", "github.com"},
		{"http://git.example.com/owner/repo.git", "git.example.com"},
		{"git@github.com:owner/repo.git", "github.com"},
		{"ssh://git@gitlab.com/owner/repo.git", "gitlab.com"},
	}
	for _, tt := range tests {
		got := cloneErrorHost(Source{CloneURL: tt.url})
		if got != tt.want {
			t.Errorf("cloneErrorHost(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestSchemaWarning(t *testing.T) {
	if w := SchemaWarning(&ServiceManifest{SchemaVersion: 0}); w != "" {
		t.Errorf("schema_version 0 should be OK, got %q", w)
	}
	if w := SchemaWarning(&ServiceManifest{SchemaVersion: CurrentSchemaVersion}); w != "" {
		t.Errorf("current schema_version should be OK, got %q", w)
	}
	if w := SchemaWarning(&ServiceManifest{SchemaVersion: CurrentSchemaVersion + 1}); w == "" {
		t.Error("a future schema_version should produce a warning")
	}
}

func TestLockfileUpsert(t *testing.T) {
	// Hermetic: point ConfigDir at a temp HOME (Linux/darwin non-root path).
	t.Setenv("HOME", t.TempDir())

	// Empty lockfile when none exists.
	lf, err := LoadLockfile()
	if err != nil {
		t.Fatalf("LoadLockfile (empty): %v", err)
	}
	if len(lf.Modules) != 0 {
		t.Fatalf("expected empty lockfile, got %d modules", len(lf.Modules))
	}

	// Insert two entries.
	if err := UpsertLockEntry(LockEntry{Name: "a", Source: "owner/a", Commit: "c1"}); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if err := UpsertLockEntry(LockEntry{Name: "b", Source: "owner/b", Commit: "c2"}); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	// Replace one in place; the other must survive.
	if err := UpsertLockEntry(LockEntry{Name: "a", Source: "owner/a", Commit: "c1-new"}); err != nil {
		t.Fatalf("upsert a (replace): %v", err)
	}

	lf, err = LoadLockfile()
	if err != nil {
		t.Fatalf("LoadLockfile (after): %v", err)
	}
	if len(lf.Modules) != 2 {
		t.Fatalf("expected 2 modules after replace, got %d", len(lf.Modules))
	}
	ea, ok := lf.LookupLock("a")
	if !ok || ea.Commit != "c1-new" {
		t.Errorf("entry a not replaced: %+v (ok=%v)", ea, ok)
	}
	eb, ok := lf.LookupLock("b")
	if !ok || eb.Commit != "c2" {
		t.Errorf("entry b not preserved: %+v (ok=%v)", eb, ok)
	}
}

func TestContainerNameConflict_EmptyAndNoDocker(t *testing.T) {
	// Empty name is never a conflict.
	if ContainerNameConflict("") {
		t.Error("empty container name should never conflict")
	}
}
