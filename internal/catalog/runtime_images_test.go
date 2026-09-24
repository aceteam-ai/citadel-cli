package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRegistryRuntimeImagesOnlyFromTrustedDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	defaultRegistry := `version: 1
runtime_images:
  - name: python
    image: ghcr.io/aceteam-ai/aceteam-app-python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    discovery_alias: ghcr.io/aceteam-ai/aceteam-app-python:stable
    architectures: [amd64, arm64]
services: []
`
	writeRegistryForRuntimeTest(t, GetCatalogPath(), defaultRegistry)

	if err := AddSource("community", "https://github.com/example/community.git"); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	communityRegistry := `version: 1
runtime_images:
  - name: attacker
    image: ghcr.io/example/attacker:latest
    architectures: [amd64]
services: []
`
	writeRegistryForRuntimeTest(t, sourceCachePath("community"), communityRegistry)

	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(reg.RuntimeImages) != 1 {
		t.Fatalf("RuntimeImages = %+v, want one trusted entry", reg.RuntimeImages)
	}
	if got := reg.RuntimeImages[0].Image; got != "ghcr.io/aceteam-ai/aceteam-app-python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("trusted runtime image = %q", got)
	}
}

func TestValidateRuntimeImagesRejectsUntrustedOrMismatchedReferences(t *testing.T) {
	tests := []struct {
		name  string
		image RuntimeImage
	}{
		{
			name:  "other namespace",
			image: RuntimeImage{Name: "python", Image: "ghcr.io/example/python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Architectures: []string{"amd64"}},
		},
		{
			name:  "name mismatch",
			image: RuntimeImage{Name: "node", Image: "ghcr.io/aceteam-ai/aceteam-app-python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Architectures: []string{"amd64"}},
		},
		{
			name:  "mutable stable tag",
			image: RuntimeImage{Name: "python", Image: "ghcr.io/aceteam-ai/aceteam-app-python:stable", Architectures: []string{"amd64"}},
		},
		{
			name:  "unsupported architecture",
			image: RuntimeImage{Name: "python", Image: "ghcr.io/aceteam-ai/aceteam-app-python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Architectures: []string{"s390x"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateRuntimeImages([]RuntimeImage{test.image}); err == nil {
				t.Fatal("validateRuntimeImages returned nil error")
			}
		})
	}
}

func TestValidateRuntimeImagesRejectsDuplicateNamesAndArchitectures(t *testing.T) {
	base := RuntimeImage{
		Name:           "python",
		Image:          "ghcr.io/aceteam-ai/aceteam-app-python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DiscoveryAlias: "ghcr.io/aceteam-ai/aceteam-app-python:stable",
		Architectures:  []string{"amd64", "arm64"},
	}
	if _, err := validateRuntimeImages([]RuntimeImage{base, base}); err == nil {
		t.Fatal("duplicate name accepted")
	}

	base.Architectures = []string{"amd64", "amd64"}
	if _, err := validateRuntimeImages([]RuntimeImage{base}); err == nil {
		t.Fatal("duplicate architecture accepted")
	}
}

func TestValidateRuntimeImagesRequiresMatchingDiscoveryAlias(t *testing.T) {
	base := RuntimeImage{
		Name:          "python",
		Image:         "ghcr.io/aceteam-ai/aceteam-app-python@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Architectures: []string{"amd64"},
	}
	if _, err := validateRuntimeImages([]RuntimeImage{base}); err == nil {
		t.Fatal("missing discovery alias accepted")
	}
	base.DiscoveryAlias = "ghcr.io/aceteam-ai/aceteam-app-node:stable"
	if _, err := validateRuntimeImages([]RuntimeImage{base}); err == nil {
		t.Fatal("mismatched discovery alias accepted")
	}
}

func writeRegistryForRuntimeTest(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir registry dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(contents), 0644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
}
