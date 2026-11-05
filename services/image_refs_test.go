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
var digestPinnedComposeFiles = []string{
	"compose/claudecode.yml",
	"compose/diffusers.yml",
	"compose/hermes.yml",
	"compose/kokoro.yml",
	"compose/transcribe.yml",
}

// These exact OCI index digests were matched to successful main-build tags.
// The Kokoro publisher uses a short sha- tag; its full source commit and build
// run are recorded in docs/2026-10-sidecar-image-pins.md.
var reviewedBreachImageRefs = map[string]string{
	"compose/claudecode.yml": "ghcr.io/aceteam-ai/claudecode-service:6fad6be3951e28d6a4ad34a6e39e8c90113328ae@sha256:c2285d2bfe1db43086e240c4c95b3f605bbf83f87579fa29d1b97bade77feee2",
	"compose/hermes.yml":     "ghcr.io/aceteam-ai/hermes-service:6fad6be3951e28d6a4ad34a6e39e8c90113328ae@sha256:f8ba9d904b9330a76dc86363f00f13d0261273faba74688a44d59eda95772617",
	"compose/kokoro.yml":     "ghcr.io/aceteam-ai/kokoro-service:sha-ad043e8@sha256:0f99603bc21c89b5bfbc941eaec0329805ba52b923b05414c26a111636452241",
	"compose/transcribe.yml": "ghcr.io/aceteam-ai/whisper-service:6fad6be3951e28d6a4ad34a6e39e8c90113328ae@sha256:becb1051fcbc1fbd0fc5cc34b2099a7915411311350e9e2a1c28fda283c07f66",
}

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
			if want, reviewed := reviewedBreachImageRefs[path]; reviewed {
				if svc.Image != want {
					t.Errorf("%s service %q image is not the reviewed commit and OCI index digest", path, name)
				}
			} else if !digestPinnedImageRef.MatchString(svc.Image) {
				t.Errorf("%s service %q image %q must be ghcr.io/aceteam-ai/<name>:<commit-sha>@sha256:<digest>", path, name, svc.Image)
			}
		}
	}
}

// The NVR module includes third-party images outside #1396's scope, so apply
// the own-sidecar contract only to its init service, not every module service.
func TestNVRConfigInitImageIsPinnedAndCacheUsable(t *testing.T) {
	var doc struct {
		Services map[string]struct {
			Image      string `yaml:"image"`
			PullPolicy string `yaml:"pull_policy"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(nvrComposeYAML, &doc); err != nil {
		t.Fatal(err)
	}
	init, exists := doc.Services["nvr-config"]
	if !exists {
		t.Fatal("NVR init service is missing")
	}
	// Verified registry/CI evidence and the native/catalog gates are documented
	// in docs/nvr-config-image-pin.md. Syntax alone cannot establish provenance.
	const source = "2241cf4d5a171a0b11b733b84ec06035f185ab85"
	const digest = "dcbe4f783eb14c7979d93db68ae9239eada5fec09f71d5966aa733b20b2eebba"
	want := "ghcr.io/aceteam-ai/nvr-config:" + source + "@sha256:" + digest
	if !digestPinnedImageRef.MatchString(init.Image) || init.Image != want {
		t.Fatal("NVR init image is not the reviewed security-repair commit/digest")
	}
	if init.PullPolicy != "missing" {
		t.Fatal("NVR init image must reuse the pinned cache without a mandatory refresh")
	}
}
