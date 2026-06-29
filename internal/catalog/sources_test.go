package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateSourceName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "community", false},
		{"with-dash", "my-repo", false},
		{"with-underscore-dot", "my_repo.v2", false},
		{"empty", "", true},
		{"reserved-default", DefaultSourceName, true},
		{"path-traversal", "../evil", true},
		{"slash", "a/b", true},
		{"space", "has space", true},
		{"semicolon", "a;b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSourceName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSourceName(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSourceURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"https", "https://github.com/foo/bar.git", false},
		{"owner-repo", "foo/bar", false},
		{"scp-form", "git@github.com:foo/bar.git", false},
		{"empty", "", true},
		{"bare-name", "justaname", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSourceURL(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSourceURL(%q) err = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestDefaultSourceNameFromURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://github.com/foo/bar.git", "bar"},
		{"foo/bar", "bar"},
		{"foo/bar@v1.0.0", "bar"},
		{"git@github.com:foo/baz.git", "baz"},
		{"https://gitlab.com/group/sub/myrepo", "myrepo"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := DefaultSourceNameFromURL(tt.input); got != tt.want {
				t.Errorf("DefaultSourceNameFromURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestAddRemoveListSources(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Initially, only the implicit default source is present.
	sources, err := ListSources()
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 1 || sources[0].Name != DefaultSourceName {
		t.Fatalf("expected only the default source, got %+v", sources)
	}

	// Add a source.
	if err := AddSource("community", "https://github.com/foo/bar.git"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}

	sources, err = ListSources()
	if err != nil {
		t.Fatalf("ListSources after add: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("expected 2 sources, got %d: %+v", len(sources), sources)
	}
	if sources[0].Name != DefaultSourceName {
		t.Errorf("default source must be listed first, got %q", sources[0].Name)
	}
	if sources[1].Name != "community" {
		t.Errorf("added source name = %q, want community", sources[1].Name)
	}

	// Persistence: a fresh LoadSources reads it back.
	added, err := LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(added) != 1 || added[0].Name != "community" {
		t.Errorf("LoadSources = %+v, want one 'community' entry", added)
	}

	// Duplicate name is rejected.
	if err := AddSource("community", "https://github.com/other/repo.git"); err == nil {
		t.Error("expected duplicate-name error, got nil")
	}

	// Adding the reserved default name is rejected.
	if err := AddSource(DefaultSourceName, "https://github.com/x/y.git"); err == nil {
		t.Error("expected reserved-name error, got nil")
	}

	// Invalid name / URL rejected.
	if err := AddSource("../evil", "https://github.com/x/y.git"); err == nil {
		t.Error("expected invalid-name error, got nil")
	}
	if err := AddSource("ok", "not-a-repo"); err == nil {
		t.Error("expected invalid-url error, got nil")
	}

	// Removing the default is rejected.
	if err := RemoveSource(DefaultSourceName); err == nil {
		t.Error("expected error removing default source, got nil")
	}

	// Removing an unknown source is an error.
	if err := RemoveSource("nope"); err == nil {
		t.Error("expected error removing unknown source, got nil")
	}

	// Remove the real source.
	if err := RemoveSource("community"); err != nil {
		t.Fatalf("RemoveSource: %v", err)
	}
	sources, err = ListSources()
	if err != nil {
		t.Fatalf("ListSources after remove: %v", err)
	}
	if len(sources) != 1 {
		t.Errorf("expected only default after remove, got %+v", sources)
	}
}

func TestMergeRegistriesCollisionPrecedence(t *testing.T) {
	regs := []sourceRegistry{
		{
			Source: DefaultSourceName,
			Services: []RegistryEntry{
				{Name: "vllm", Version: "1.0.0", Description: "official vllm"},
				{Name: "shared", Version: "1.0.0", Description: "official shared"},
			},
		},
		{
			Source: "community",
			Services: []RegistryEntry{
				{Name: "shared", Version: "9.9.9", Description: "community shared (should lose)"},
				{Name: "extra", Version: "2.0.0", Description: "community extra"},
			},
		},
	}

	merged := mergeRegistries(regs)

	// Expect 3 unique services: vllm, shared, extra (sorted by name).
	if len(merged) != 3 {
		t.Fatalf("expected 3 merged services, got %d: %+v", len(merged), merged)
	}

	byName := make(map[string]RegistryEntry)
	for _, e := range merged {
		byName[e.Name] = e
	}

	// Collision: the default source wins.
	shared, ok := byName["shared"]
	if !ok {
		t.Fatal("merged result missing 'shared'")
	}
	if shared.Source != DefaultSourceName {
		t.Errorf("collision winner Source = %q, want %q", shared.Source, DefaultSourceName)
	}
	if shared.Version != "1.0.0" {
		t.Errorf("collision winner Version = %q, want the default's 1.0.0", shared.Version)
	}

	// Non-colliding entries keep their owning source.
	if byName["vllm"].Source != DefaultSourceName {
		t.Errorf("vllm Source = %q, want default", byName["vllm"].Source)
	}
	if byName["extra"].Source != "community" {
		t.Errorf("extra Source = %q, want community", byName["extra"].Source)
	}

	// Sorted by name.
	if merged[0].Name != "extra" || merged[1].Name != "shared" || merged[2].Name != "vllm" {
		t.Errorf("merged order = [%s %s %s], want [extra shared vllm]",
			merged[0].Name, merged[1].Name, merged[2].Name)
	}
}

func TestMergeRegistriesRegistrationOrder(t *testing.T) {
	// Two community sources both define "dup"; the first-registered wins.
	regs := []sourceRegistry{
		{Source: "first", Services: []RegistryEntry{{Name: "dup", Version: "1"}}},
		{Source: "second", Services: []RegistryEntry{{Name: "dup", Version: "2"}}},
	}
	merged := mergeRegistries(regs)
	if len(merged) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(merged))
	}
	if merged[0].Source != "first" || merged[0].Version != "1" {
		t.Errorf("got source=%q version=%q, want first/1", merged[0].Source, merged[0].Version)
	}
}

// TestCrossSourceAggregationOnDisk seeds two source caches on disk (the default
// at GetCatalogPath() and an added source at its subdir) and verifies that the
// public readers aggregate across them with the documented collision precedence.
func TestCrossSourceAggregationOnDisk(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Seed the default source: services "vllm" and "shared".
	seedService(t, GetCatalogPath(), "vllm", "name: vllm\nversion: 1.0.0\ndescription: official vllm\ncategory: inference\n")
	seedService(t, GetCatalogPath(), "shared", "name: shared\nversion: 1.0.0\ndescription: official shared\ncategory: misc\n")

	// Register and seed a community source: services "shared" (collision) and "extra".
	if err := AddSource("community", "https://github.com/foo/bar.git"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	communityPath := sourceCachePath("community")
	seedService(t, communityPath, "shared", "name: shared\nversion: 9.9.9\ndescription: community shared\ncategory: misc\n")
	seedService(t, communityPath, "extra", "name: extra\nversion: 2.0.0\ndescription: community extra\ncategory: tools\n")

	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(reg.Services) != 3 {
		t.Fatalf("expected 3 aggregated services, got %d: %+v", len(reg.Services), reg.Services)
	}

	byName := make(map[string]RegistryEntry)
	for _, e := range reg.Services {
		byName[e.Name] = e
	}

	// Collision precedence: default wins for "shared".
	if byName["shared"].Source != DefaultSourceName {
		t.Errorf("shared Source = %q, want default", byName["shared"].Source)
	}

	// "extra" came only from the community source.
	if byName["extra"].Source != "community" {
		t.Errorf("extra Source = %q, want community", byName["extra"].Source)
	}

	// LoadServiceManifest follows the same precedence (default's shared wins).
	m, err := LoadServiceManifest("shared")
	if err != nil {
		t.Fatalf("LoadServiceManifest(shared): %v", err)
	}
	if m.Version != "1.0.0" {
		t.Errorf("LoadServiceManifest(shared).Version = %q, want default's 1.0.0", m.Version)
	}

	// A service that exists only in the community source resolves from it.
	if _, err := LoadServiceManifest("extra"); err != nil {
		t.Errorf("LoadServiceManifest(extra): %v", err)
	}

	// GetComposeFile finds the community-only service's compose.
	composePath, err := GetComposeFile("extra")
	if err != nil {
		t.Fatalf("GetComposeFile(extra): %v", err)
	}
	if filepath.Dir(filepath.Dir(filepath.Dir(composePath))) != communityPath {
		t.Errorf("compose for extra not under community source path: %s", composePath)
	}

	// Search spans sources.
	results, err := Search("community extra")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, r := range results {
		if r.Name == "extra" {
			found = true
		}
	}
	if !found {
		t.Errorf("Search did not surface community-source service 'extra'")
	}
}

func TestSourceOfAndTrust(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	seedService(t, GetCatalogPath(), "official-svc", "name: official-svc\nversion: 1.0.0\n")
	if err := AddSource("community", "https://github.com/foo/bar.git"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	seedService(t, sourceCachePath("community"), "community-svc", "name: community-svc\nversion: 1.0.0\n")

	if got := SourceOf("official-svc"); got != DefaultSourceName {
		t.Errorf("SourceOf(official-svc) = %q, want %q", got, DefaultSourceName)
	}
	if got := SourceOf("community-svc"); got != "community" {
		t.Errorf("SourceOf(community-svc) = %q, want community", got)
	}
	if got := SourceOf("does-not-exist"); got != "" {
		t.Errorf("SourceOf(missing) = %q, want empty", got)
	}

	if !IsDefaultSource(SourceOf("official-svc")) {
		t.Error("official-svc should be a default-source (trusted) service")
	}
	if IsDefaultSource(SourceOf("community-svc")) {
		t.Error("community-svc should NOT be a default-source service")
	}
	// Empty == default for backward compatibility.
	if !IsDefaultSource("") {
		t.Error("empty source name should be treated as the default (trusted)")
	}
}

// TestResolveCatalogServiceAtomic verifies that manifest, compose, and source
// are resolved from the SAME source, including the shadow scenario where the
// default source has the name as host-provisioned (service.yaml, no compose) and
// a community source defines the same name WITH a compose. The owning source for
// trust purposes must be the default (first by precedence), and its compose must
// be empty -- so the install path treats it as host-provisioned and never
// installs the community compose under default privileges.
func TestResolveCatalogServiceAtomic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Default source: "wechat" host-provisioned (service.yaml only, no compose.yml).
	hostProvDir := filepath.Join(GetCatalogPath(), servicesSubdir, "wechat")
	if err := os.MkdirAll(hostProvDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hostProvDir, "service.yaml"), []byte("name: wechat\nversion: 1.0.0\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Community source: "wechat" WITH a privileged compose (the attack).
	if err := AddSource("evilcat", "https://github.com/evil/cat.git"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	evilDir := filepath.Join(sourceCachePath("evilcat"), servicesSubdir, "wechat")
	if err := os.MkdirAll(evilDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(evilDir, "service.yaml"), []byte("name: wechat\nversion: 9.9.9\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(evilDir, "compose.yml"), []byte("services:\n  wechat:\n    privileged: true\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	resolved, err := ResolveCatalogService("wechat")
	if err != nil {
		t.Fatalf("ResolveCatalogService: %v", err)
	}
	// Owning source must be the default (precedence), NOT the community source.
	if resolved.SourceName != DefaultSourceName {
		t.Errorf("resolved source = %q, want %q (default wins, no shadow)", resolved.SourceName, DefaultSourceName)
	}
	// Compose must come from the SAME (default) source, which has none -> empty,
	// so the install path treats it as host-provisioned (not the evil compose).
	if resolved.ComposePath != "" {
		t.Errorf("resolved compose = %q, want empty (default has no compose for wechat)", resolved.ComposePath)
	}
	if resolved.Manifest.Version != "1.0.0" {
		t.Errorf("resolved manifest version = %q, want default's 1.0.0", resolved.Manifest.Version)
	}
}

// TestResolveCatalogServiceCommunityRoutesUntrusted verifies that a service
// existing ONLY in a community source resolves to that source (so the install
// path routes it as untrusted).
func TestResolveCatalogServiceCommunityRoutesUntrusted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	seedService(t, GetCatalogPath(), "official-only", "name: official-only\nversion: 1.0.0\n")
	if err := AddSource("community", "https://github.com/foo/bar.git"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	seedService(t, sourceCachePath("community"), "community-only", "name: community-only\nversion: 2.0.0\n")

	resolved, err := ResolveCatalogService("community-only")
	if err != nil {
		t.Fatalf("ResolveCatalogService: %v", err)
	}
	if resolved.SourceName != "community" {
		t.Errorf("resolved source = %q, want community", resolved.SourceName)
	}
	if IsDefaultSource(resolved.SourceName) {
		t.Error("community-only must NOT route as trusted")
	}
	if resolved.ComposePath == "" {
		t.Error("expected a compose path for the community service")
	}
}

// TestCommunityPrivilegedInstallRefused verifies that a community-source compose
// requesting privileged access is refused when installed via the untrusted path
// (allowPrivileged=false) -- the security routing this feature relies on.
func TestCommunityPrivilegedInstallRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	manifest := &ServiceManifest{Name: "evil", Version: "1.0.0"}
	composeSrc := filepath.Join(t.TempDir(), "compose.yml")
	if err := os.WriteFile(composeSrc, []byte("services:\n  evil:\n    image: evil:latest\n    privileged: true\n"), 0644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	servicesDir := filepath.Join(t.TempDir(), "services")

	// Untrusted install with allowPrivileged=false must refuse.
	_, err := InstallFromManifest(manifest, composeSrc, servicesDir, nil, false, false, true)
	if err == nil {
		t.Fatal("expected refusal installing a privileged community compose, got nil")
	}

	// Same compose as a trusted (default) install is allowed (allowPrivileged=true).
	if _, err := InstallFromManifest(manifest, composeSrc, servicesDir, nil, false, true, false); err != nil {
		t.Errorf("trusted install of privileged compose should succeed, got %v", err)
	}
}

// TestLoadRegistryResilientToBadSource verifies that one malformed community
// source does not take down the whole catalog: the default source's services
// still load.
func TestLoadRegistryResilientToBadSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	seedService(t, GetCatalogPath(), "good-svc", "name: good-svc\nversion: 1.0.0\n")

	if err := AddSource("broken", "https://github.com/foo/broken.git"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	// Seed the broken source dir with a malformed registry.yaml and no services dir.
	brokenPath := sourceCachePath("broken")
	if err := os.MkdirAll(brokenPath, 0755); err != nil {
		t.Fatalf("mkdir broken: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brokenPath, "registry.yaml"), []byte("::not yaml::\n  - bad"), 0644); err != nil {
		t.Fatalf("write bad registry: %v", err)
	}

	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry should not hard-fail on a bad source: %v", err)
	}
	found := false
	for _, e := range reg.Services {
		if e.Name == "good-svc" {
			found = true
		}
	}
	if !found {
		t.Errorf("default source's good-svc missing after a broken community source; got %+v", reg.Services)
	}
}

// seedService writes a minimal service.yaml + compose.yml under
// <catalogPath>/services/<name>/, mirroring the on-disk layout of a cloned source.
func seedService(t *testing.T, catalogPath, name, manifest string) {
	t.Helper()
	svcDir := filepath.Join(catalogPath, servicesSubdir, name)
	if err := os.MkdirAll(svcDir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", svcDir, err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "service.yaml"), []byte(manifest), 0644); err != nil {
		t.Fatalf("write service.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "compose.yml"), []byte("services: {}\n"), 0644); err != nil {
		t.Fatalf("write compose.yml: %v", err)
	}
}
