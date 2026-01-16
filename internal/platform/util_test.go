// internal/platform/util_test.go
package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidatePathWithinDir(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "citadel-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create subdirectory
	subDir := filepath.Join(tmpDir, "services")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}

	tests := []struct {
		name         string
		baseDir      string
		relativePath string
		wantErr      bool
		errContains  string
	}{
		{
			name:         "valid relative path",
			baseDir:      tmpDir,
			relativePath: "services/vllm.yml",
			wantErr:      false,
		},
		{
			name:         "valid simple filename",
			baseDir:      tmpDir,
			relativePath: "config.yaml",
			wantErr:      false,
		},
		{
			name:         "valid nested path",
			baseDir:      tmpDir,
			relativePath: "services/docker/compose.yml",
			wantErr:      false,
		},
		{
			name:         "path traversal with ..",
			baseDir:      tmpDir,
			relativePath: "../../../etc/passwd",
			wantErr:      true,
			errContains:  "path traversal detected",
		},
		{
			name:         "path traversal with leading ..",
			baseDir:      tmpDir,
			relativePath: "../../secret.yml",
			wantErr:      true,
			errContains:  "path traversal detected",
		},
		{
			name:         "path traversal in middle",
			baseDir:      tmpDir,
			relativePath: "services/../../../etc/passwd",
			wantErr:      true,
			errContains:  "path traversal detected",
		},
		{
			name:         "absolute-looking path stays within base",
			baseDir:      tmpDir,
			relativePath: "/etc/passwd",
			wantErr:      false, // filepath.Join treats this as relative on Unix
		},
		{
			name:         "valid path with safe ..",
			baseDir:      tmpDir,
			relativePath: "services/../config.yml",
			wantErr:      false, // This stays within base dir
		},
		{
			name:         "empty relative path",
			baseDir:      tmpDir,
			relativePath: "",
			wantErr:      false, // Empty path resolves to base dir
		},
		{
			name:         "dot path",
			baseDir:      tmpDir,
			relativePath: ".",
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidatePathWithinDir(tt.baseDir, tt.relativePath)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ValidatePathWithinDir() expected error but got none, result: %s", result)
				} else if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ValidatePathWithinDir() error = %v, want error containing %q", err, tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("ValidatePathWithinDir() unexpected error: %v", err)
				}
				// Verify the result is within the base directory
				absBase, _ := filepath.Abs(tt.baseDir)
				if result != "" && !isWithinDir(result, absBase) {
					t.Errorf("ValidatePathWithinDir() result %q is not within base %q", result, absBase)
				}
			}
		})
	}
}

func TestValidatePathWithinDir_PrefixAttack(t *testing.T) {
	// Test that /home/user doesn't match /home/username
	// Create two temp dirs with similar names
	tmpBase, err := os.MkdirTemp("", "citadel-base-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpBase)

	// Create a sibling directory with a longer name
	siblingDir := tmpBase + "-sibling"
	if err := os.MkdirAll(siblingDir, 0755); err != nil {
		t.Fatalf("Failed to create sibling dir: %v", err)
	}
	defer os.RemoveAll(siblingDir)

	// Try to escape using the sibling directory name pattern
	// This tests that we don't match /tmp/citadel-base-xxx when trying to access
	// /tmp/citadel-base-xxx-sibling
	_, err = ValidatePathWithinDir(tmpBase, "../"+filepath.Base(siblingDir)+"/secret.txt")
	if err == nil {
		t.Error("ValidatePathWithinDir() should detect escape to sibling directory")
	}
}

// Helper function to check if path is within directory
func isWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	// Check that the relative path doesn't start with ".."
	return len(rel) > 0 && rel[0] != '.' || rel == "."
}
