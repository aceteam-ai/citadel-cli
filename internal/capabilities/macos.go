//go:build darwin

package capabilities

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

// detectXcode checks for Xcode installation via xcode-select and returns the
// developer directory path, or empty string if not installed.
func detectXcode() string {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xcode-select", "-p").Output()
	if err != nil {
		return ""
	}
	path := parseXcodePath(string(out))
	if path == "" {
		return ""
	}
	// Verify the directory exists
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

// detectXcodeVersion runs xcodebuild -version and returns the parsed version string.
func detectXcodeVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xcodebuild", "-version").Output()
	if err != nil {
		return ""
	}
	return parseXcodeVersion(string(out))
}

// detectAndroidSDK checks for Android SDK installation via ANDROID_HOME or default paths.
func detectAndroidSDK() string {
	// Check ANDROID_HOME env var first
	if home := os.Getenv("ANDROID_HOME"); home != "" {
		if isValidAndroidSDK(home) {
			return home
		}
	}
	// Check ANDROID_SDK_ROOT (older convention)
	if root := os.Getenv("ANDROID_SDK_ROOT"); root != "" {
		if isValidAndroidSDK(root) {
			return root
		}
	}
	// Check default macOS paths
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	defaults := []string{
		filepath.Join(homeDir, "Library", "Android", "sdk"),
	}
	for _, p := range defaults {
		if isValidAndroidSDK(p) {
			return p
		}
	}
	return ""
}
