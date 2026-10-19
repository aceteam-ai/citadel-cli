package services

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

// digestPinnedComposeFiles lists the compose files whose own-built sidecar
// image must be pinned by digest (citadel-cli#1346). `docker compose up` pulls
// only a missing image, so a floating tag already present on a node is never
// refreshed, and the node keeps running an image older than the compose
// defaults this binary ships. Add a file here when its image is pinned.
var digestPinnedComposeFiles = []string{"compose/diffusers.yml"}

var digestPinnedImageRef = regexp.MustCompile(`^ghcr\.io/aceteam-ai/[a-z0-9-]+:[0-9a-f]{40}@sha256:[0-9a-f]{64}$`)

func TestOwnSidecarImagesArePinnedByDigest(t *testing.T) {
	for _, path := range digestPinnedComposeFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Image string `yaml:"image"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: parse YAML: %v", path, err)
		}
		if len(doc.Services) == 0 {
			t.Fatalf("%s: no services", path)
		}
		for name, svc := range doc.Services {
			if !digestPinnedImageRef.MatchString(svc.Image) {
				t.Errorf("%s service %q image %q must be ghcr.io/aceteam-ai/<name>:<commit-sha>@sha256:<digest>", path, name, svc.Image)
			}
		}
	}
}
