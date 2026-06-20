package capabilities

import (
	"os"
	"path/filepath"
	"strings"
)

// parseXcodePath extracts the developer directory path from xcode-select -p output.
func parseXcodePath(output string) string {
	return strings.TrimSpace(output)
}

// parseXcodeVersion extracts the version string from xcodebuild -version output.
// Input typically looks like:
//
//	Xcode 15.4
//	Build version 15F31d
//
// Returns "15.4" (the version number only).
func parseXcodeVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Xcode ") {
			return strings.TrimPrefix(line, "Xcode ")
		}
	}
	return ""
}

// isValidAndroidSDK checks whether a path looks like a valid Android SDK directory
// by checking for the presence of characteristic subdirectories.
func isValidAndroidSDK(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	// Check for at least one of the expected SDK subdirectories
	markers := []string{"platform-tools", "build-tools", "platforms"}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(path, m)); err == nil {
			return true
		}
	}
	return false
}

// DetectMacOSToolchains detects macOS-specific developer toolchains and returns
// capability tags. This function is called from DetectNodeCapabilities.
func DetectMacOSToolchains() []MacOSToolchain {
	var tools []MacOSToolchain

	if xcodePath := detectXcode(); xcodePath != "" {
		version := detectXcodeVersion()
		tools = append(tools, MacOSToolchain{
			Name:    "xcode",
			Tag:     "tool:xcode",
			Path:    xcodePath,
			Version: version,
		})
	}

	if sdkPath := detectAndroidSDK(); sdkPath != "" {
		tools = append(tools, MacOSToolchain{
			Name: "android-sdk",
			Tag:  "tool:android-sdk",
			Path: sdkPath,
		})
	}

	return tools
}

// MacOSToolchain holds information about a detected macOS developer toolchain.
type MacOSToolchain struct {
	Name    string // short name (e.g. "xcode", "android-sdk")
	Tag     string // capability tag (e.g. "tool:xcode")
	Path    string // installation path
	Version string // version string (optional)
}
