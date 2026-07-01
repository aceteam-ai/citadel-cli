package services

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestComposeFilesAreValidYAML verifies that all embedded compose files
// parse as valid YAML. This catches syntax errors at test time rather
// than at runtime when a user runs `citadel run <service>`.
func TestComposeFilesAreValidYAML(t *testing.T) {
	for name, content := range ServiceMap {
		t.Run(name, func(t *testing.T) {
			var parsed map[string]any
			if err := yaml.Unmarshal([]byte(content), &parsed); err != nil {
				t.Errorf("compose file for %q is not valid YAML: %v", name, err)
			}
			// Every compose file should have a top-level "services" key
			if _, ok := parsed["services"]; !ok {
				t.Errorf("compose file for %q missing top-level 'services' key", name)
			}
		})
	}
}

// TestSGLangComposeRegistered ensures the sglang service is in the ServiceMap.
func TestSGLangComposeRegistered(t *testing.T) {
	if _, ok := ServiceMap["sglang"]; !ok {
		t.Fatal("sglang not found in ServiceMap")
	}
}

// TestGetAvailableServicesIncludesSGLang ensures sglang appears in the
// sorted available services list.
func TestGetAvailableServicesIncludesSGLang(t *testing.T) {
	available := GetAvailableServices()
	found := false
	for _, s := range available {
		if s == "sglang" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAvailableServices() = %v, want sglang to be included", available)
	}
}

// TestDiffusersComposeRegistered ensures the diffusers service is in the
// ServiceMap so `citadel init` writes services/diffusers.yml and a node can
// enable it (aceteam #4468).
func TestDiffusersComposeRegistered(t *testing.T) {
	if _, ok := ServiceMap["diffusers"]; !ok {
		t.Fatal("diffusers not found in ServiceMap")
	}
}

// TestGetAvailableServicesIncludesDiffusers ensures diffusers appears in the
// sorted available services list.
func TestGetAvailableServicesIncludesDiffusers(t *testing.T) {
	available := GetAvailableServices()
	found := false
	for _, s := range available {
		if s == "diffusers" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GetAvailableServices() = %v, want diffusers to be included", available)
	}
}

// TestDiffusersComposeContract verifies the embedded diffusers compose file
// satisfies the aceteam #4468 contract: the container listens on the contract
// port 7860 and the host mapping avoids the terminal server's default 7860.
func TestDiffusersComposeContract(t *testing.T) {
	content := ServiceMap["diffusers"]
	// Container/server port is the contract port.
	if !strings.Contains(content, ":7860") {
		t.Errorf("diffusers compose should map to container port 7860; got:\n%s", content)
	}
	// Host port must not be 7860 (that would collide with the terminal server).
	if strings.Contains(content, "\"7860:") {
		t.Errorf("diffusers compose host port must not be 7860 (collides with terminal server); got:\n%s", content)
	}
}
