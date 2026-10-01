package services

import (
	"os"
	"strings"
	"testing"
)

// TestSpirulaRuntimeImageContract protects the loader and licensing paths.
// libvulkan alone is insufficient: the injected NVIDIA ICD needs libEGL to
// resolve vkCreateInstance.
func TestSpirulaRuntimeImageContract(t *testing.T) {
	dockerfile, err := os.ReadFile("spirula-reconstruct/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerfile)
	for _, dependency := range []string{"libegl1", "libopengl0", "libvulkan1", "vulkan-tools"} {
		if !strings.Contains(contents, dependency) {
			t.Errorf("Spirula runtime image is missing NVIDIA Vulkan dependency %q", dependency)
		}
	}
	if !strings.Contains(contents, "NVIDIA_DRIVER_CAPABILITIES=graphics,display,utility") {
		t.Error("Spirula runtime image must request NVIDIA graphics/display capabilities")
	}
	if !strings.Contains(contents, "COPY LICENSE /usr/share/licenses/citadel-cli/LICENSE") {
		t.Error("Spirula runtime image must include the Citadel Elastic-2.0 license")
	}
	if !strings.Contains(contents, `org.opencontainers.image.licenses="Elastic-2.0 AND GPL-3.0-only"`) {
		t.Error("Spirula runtime image must accurately label both distributed licenses")
	}

	notice, err := os.ReadFile("spirula-reconstruct/THIRD_PARTY_NOTICES.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notice), "Elastic-2.0 Citadel orchestration binary") ||
		strings.Contains(string(notice), "Apache-2.0") {
		t.Error("Spirula third-party notice must describe the Citadel binary as Elastic-2.0")
	}
}

// TestSpirulaComposeMountsInputBelowParent prevents a video-file bind from
// colliding with the /input directory baked into the image. The child target
// may be populated by either a file or a frame directory.
func TestSpirulaComposeMountsInputBelowParent(t *testing.T) {
	compose, err := os.ReadFile("spirula-reconstruct/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(compose)
	if !strings.Contains(contents, ":/input/source:ro") {
		t.Error("Spirula source must bind below the image's pre-created /input directory")
	}
	if !strings.Contains(contents, "- /input/source") {
		t.Error("Spirula command must consume the file-or-directory child bind target")
	}
}
