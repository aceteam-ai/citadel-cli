// cmd/up_test.go
package cmd

import (
	"errors" // <-- IMPORT THIS
	"os"
	"testing"
)

func TestReadManifest(t *testing.T) {
	t.Run("ValidManifest", func(t *testing.T) {
		content := `
name: test-node-valid
tags: [gpu, test]
services:
  - name: test-service
    compose_file: docker-compose.test.yml
`
		tmpFile, err := os.CreateTemp("", "citadel-manifest-*.yaml")
		if err != nil {
			t.Fatalf("Failed to create temp file: %v", err)
		}
		defer os.Remove(tmpFile.Name())

		if _, err := tmpFile.Write([]byte(content)); err != nil {
			t.Fatalf("Failed to write to temp file: %v", err)
		}
		tmpFile.Close()

		manifest, err := readManifest(tmpFile.Name())
		if err != nil {
			t.Errorf("readManifest() returned an unexpected error: %v", err)
		}

		if manifest.Name != "test-node-valid" {
			t.Errorf("Expected manifest name 'test-node-valid', got '%s'", manifest.Name)
		}

		if len(manifest.Services) != 1 {
			t.Errorf("Expected 1 service, got %d", len(manifest.Services))
		}

		if manifest.Services[0].Name != "test-service" {
			t.Errorf("Expected service name 'test-service', got '%s'", manifest.Services[0].Name)
		}
	})

	t.Run("FileNotFound", func(t *testing.T) {
		_, err := readManifest("non-existent-file.yaml")
		if err == nil {
			t.Error("Expected an error for a non-existent file, but got nil")
		}
		// FIX: Use errors.Is() to correctly check inside wrapped errors.
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Expected a file-not-found error, but got a different error: %v", err)
		}
	})

	t.Run("InvalidYAML", func(t *testing.T) {
		content := `name: test-node-invalid: unclosed quote"`
		tmpFile, err := os.CreateTemp("", "citadel-manifest-*.yaml")
		if err != nil {
			t.Fatalf("Failed to create temp file: %v", err)
		}
		defer os.Remove(tmpFile.Name())

		if _, err := tmpFile.Write([]byte(content)); err != nil {
			t.Fatalf("Failed to write to temp file: %v", err)
		}
		tmpFile.Close()

		_, err = readManifest(tmpFile.Name())
		if err == nil {
			t.Error("Expected an error for invalid YAML, but got nil")
		}
	})
}
