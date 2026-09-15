package services

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// darwinVariant describes a macOS CPU/arm64 compose variant (citadel-cli#1048)
// and the EXACT, reviewed set of differences it is allowed to have from its
// linux original. The tests below read the files directly (not the build-tagged
// embed vars) so they run on the linux CI host even though the variant is only
// compiled into a darwin build.
type darwinVariant struct {
	engine     string // ServiceMap key / top-level service name
	linuxFile  string // filename under compose/
	darwinFile string // filename under compose/
	// If non-empty, the image tag is expected to differ: the linux file must
	// pin linuxImage and the darwin file must pin darwinImage. Empty means the
	// image must be identical (the only allowed difference is the GPU reservation).
	linuxImage  string
	darwinImage string
}

var darwinVariants = []darwinVariant{
	{
		engine:     "ollama",
		linuxFile:  "ollama.yml",
		darwinFile: "ollama.darwin.yml",
	},
	{
		engine:      "llamacpp",
		linuxFile:   "llamacpp.yml",
		darwinFile:  "llamacpp.darwin.yml",
		linuxImage:  "ghcr.io/ggml-org/llama.cpp:server-cuda",
		darwinImage: "ghcr.io/ggml-org/llama.cpp:server",
	},
}

// composeServices parses a compose file into its `services:` map of raw
// per-service key/value trees, so a test can compare structure while ignoring
// comments and key ordering.
func composeServices(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}
	if len(doc.Services) == 0 {
		t.Fatalf("%s declares no services", path)
	}
	return doc.Services
}

// TestDarwinComposeVariantsOnlyDropGPU pins that each darwin CPU/arm64 variant
// differs from its linux original by EXACTLY the GPU reservation (and, where
// declared, the image tag) and nothing else. If a future edit to the linux file
// (a new volume, env var, port, command flag) is not mirrored into the darwin
// variant, this fails — the variant cannot silently drift.
func TestDarwinComposeVariantsOnlyDropGPU(t *testing.T) {
	for _, v := range darwinVariants {
		t.Run(v.engine, func(t *testing.T) {
			linux := composeServices(t, filepath.Join("compose", v.linuxFile))
			darwin := composeServices(t, filepath.Join("compose", v.darwinFile))

			lsvc, ok := linux[v.engine]
			if !ok {
				t.Fatalf("linux %s has no service %q", v.linuxFile, v.engine)
			}
			dsvc, ok := darwin[v.engine]
			if !ok {
				t.Fatalf("darwin %s has no service %q", v.darwinFile, v.engine)
			}

			// The linux file must carry a GPU reservation; the darwin file must not.
			if _, has := lsvc["deploy"]; !has {
				t.Errorf("linux %s no longer declares a `deploy` block — is the GPU reservation still present? Update this test if the linux shape changed", v.linuxFile)
			}
			if _, has := dsvc["deploy"]; has {
				t.Errorf("darwin %s still declares a `deploy` block; the whole point of the variant is to drop the nvidia reservation", v.darwinFile)
			}
			delete(lsvc, "deploy")

			// Image: identical unless the variant declares a swap.
			if v.darwinImage != "" {
				if got := lsvc["image"]; got != v.linuxImage {
					t.Errorf("linux %s image = %v, want %q (update this test if the tag intentionally changed)", v.linuxFile, got, v.linuxImage)
				}
				if got := dsvc["image"]; got != v.darwinImage {
					t.Errorf("darwin %s image = %v, want %q", v.darwinFile, got, v.darwinImage)
				}
				// Normalize the image so DeepEqual checks everything else is equal.
				lsvc["image"] = dsvc["image"]
			}

			if !reflect.DeepEqual(lsvc, dsvc) {
				t.Errorf("darwin variant %s differs from %s by more than the GPU reservation%s:\n linux=%#v\ndarwin=%#v",
					v.darwinFile, v.linuxFile,
					map[bool]string{true: " and image tag", false: ""}[v.darwinImage != ""],
					lsvc, dsvc)
			}
		})
	}
}

// TestKnownComposeHashesCoverDarwinVariants is the darwin analogue of
// TestKnownComposeHashesCoverCurrentTemplates: on a darwin node, ServiceMap holds
// the variant content, so composerefresh materializes the variant file and — for
// an un-stamped pre-#426 node — falls back to KnownComposeHashes to decide the
// file is citadel-written rather than operator-edited. That fallback only works
// if the variant's hash is registered. This test reads the variant files
// directly so it runs on the linux CI host.
func TestKnownComposeHashesCoverDarwinVariants(t *testing.T) {
	for _, v := range darwinVariants {
		t.Run(v.engine, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("compose", v.darwinFile))
			if err != nil {
				t.Fatalf("read %s: %v", v.darwinFile, err)
			}
			sum := sha256.Sum256(raw)
			h := hex.EncodeToString(sum[:])
			if !KnownComposeHashes[v.engine][h] {
				t.Errorf("KnownComposeHashes[%q] is missing the darwin variant hash %s (from compose/%s); add it to services/known_hashes.go", v.engine, h, v.darwinFile)
			}
		})
	}
}
