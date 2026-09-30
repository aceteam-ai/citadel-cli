package services

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCheckedInComposeImagesAreRegistryQualified protects rootless Podman
// nodes, where short names do not resolve unless the host has separately opted
// into an unqualified-search registry. Docker Hub images must therefore name
// docker.io explicitly. Images produced by a compose build use Podman's
// explicit localhost/ namespace so Compose never mistakes the resulting local
// tag for a registry pull.
func TestCheckedInComposeImagesAreRegistryQualified(t *testing.T) {
	err := fs.WalkDir(os.DirFS("."), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var doc struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Errorf("%s: parse YAML: %v", path, err)
			return nil
		}

		for name, service := range doc.Services {
			image, hasImage := service["image"].(string)
			if !hasImage || strings.TrimSpace(image) == "" {
				continue
			}
			image = strings.TrimSpace(image)
			_, hasBuild := service["build"]

			if !registryQualifiedImage(image) {
				t.Errorf("%s service %q uses unqualified image %q; name the registry explicitly", path, name, image)
			}
			if hasBuild && !strings.HasPrefix(image, "localhost/") {
				t.Errorf("%s service %q builds locally but tags %q; local builds must use the localhost/ namespace", path, name, image)
			}
			if !hasBuild && strings.HasPrefix(image, "localhost/") {
				t.Errorf("%s service %q references local image %q without a build section", path, name, image)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk checked-in service manifests: %v", err)
	}
}

func registryQualifiedImage(image string) bool {
	first, _, ok := strings.Cut(image, "/")
	if !ok {
		return false
	}
	return first == "localhost" || strings.Contains(first, ".") || strings.Contains(first, ":")
}
