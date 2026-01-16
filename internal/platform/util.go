// internal/platform/util.go
package platform

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// isCommandAvailable checks if a command is available in PATH
func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// ValidatePathWithinDir validates that a relative path, when joined with baseDir,
// stays within the baseDir. This prevents path traversal attacks where a malicious
// relative path like "../../../etc/passwd" could escape the intended directory.
//
// Returns the validated absolute path if safe, or an error if path traversal is detected.
func ValidatePathWithinDir(baseDir, relativePath string) (string, error) {
	// Get absolute path of base directory
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve base directory: %w", err)
	}

	// Join and get absolute path of the target
	joinedPath := filepath.Join(absBaseDir, relativePath)
	absTargetPath, err := filepath.Abs(joinedPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve target path: %w", err)
	}

	// Ensure the resolved path is within the base directory
	// Add trailing separator to baseDir to prevent prefix matching issues
	// (e.g., /home/user matching /home/username)
	baseDirWithSep := absBaseDir + string(filepath.Separator)
	if !strings.HasPrefix(absTargetPath+string(filepath.Separator), baseDirWithSep) && absTargetPath != absBaseDir {
		return "", fmt.Errorf("path traversal detected: %q escapes base directory %q", relativePath, baseDir)
	}

	return absTargetPath, nil
}
