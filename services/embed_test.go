package services

import (
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
