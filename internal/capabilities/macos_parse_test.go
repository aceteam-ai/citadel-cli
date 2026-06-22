package capabilities

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseXcodeVersion(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "standard output",
			input:  "Xcode 15.4\nBuild version 15F31d\n",
			expect: "15.4",
		},
		{
			name:   "xcode 16",
			input:  "Xcode 16.0\nBuild version 16A242d\n",
			expect: "16.0",
		},
		{
			name:   "beta version",
			input:  "Xcode 16.1 Beta\nBuild version 16B5001e\n",
			expect: "16.1 Beta",
		},
		{
			name:   "empty input",
			input:  "",
			expect: "",
		},
		{
			name:   "no xcode line",
			input:  "Some other output\n",
			expect: "",
		},
		{
			name:   "only version line",
			input:  "Xcode 14.3.1",
			expect: "14.3.1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseXcodeVersion(tc.input)
			if got != tc.expect {
				t.Errorf("parseXcodeVersion(%q) = %q, want %q", tc.input, got, tc.expect)
			}
		})
	}
}

func TestParseXcodePath(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name:   "standard xcode",
			input:  "/Applications/Xcode.app/Contents/Developer\n",
			expect: "/Applications/Xcode.app/Contents/Developer",
		},
		{
			name:   "command line tools only",
			input:  "/Library/Developer/CommandLineTools\n",
			expect: "/Library/Developer/CommandLineTools",
		},
		{
			name:   "no trailing newline",
			input:  "/Applications/Xcode.app/Contents/Developer",
			expect: "/Applications/Xcode.app/Contents/Developer",
		},
		{
			name:   "empty",
			input:  "",
			expect: "",
		},
		{
			name:   "whitespace only",
			input:  "  \n  ",
			expect: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseXcodePath(tc.input)
			if got != tc.expect {
				t.Errorf("parseXcodePath(%q) = %q, want %q", tc.input, got, tc.expect)
			}
		})
	}
}

func TestIsValidAndroidSDK(t *testing.T) {
	// Create a temp directory that looks like an Android SDK
	tmpDir := t.TempDir()
	validSDK := filepath.Join(tmpDir, "android-sdk")
	if err := os.MkdirAll(filepath.Join(validSDK, "platform-tools"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create another with build-tools
	validSDK2 := filepath.Join(tmpDir, "android-sdk2")
	if err := os.MkdirAll(filepath.Join(validSDK2, "build-tools"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create an empty directory (not a valid SDK)
	emptyDir := filepath.Join(tmpDir, "empty")
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   string
		expect bool
	}{
		{"valid SDK with platform-tools", validSDK, true},
		{"valid SDK with build-tools", validSDK2, true},
		{"empty directory", emptyDir, false},
		{"nonexistent path", "/nonexistent/path", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isValidAndroidSDK(tc.path)
			if got != tc.expect {
				t.Errorf("isValidAndroidSDK(%q) = %v, want %v", tc.path, got, tc.expect)
			}
		})
	}
}

func TestDetectMacOSToolchains_Tags(t *testing.T) {
	// On non-macOS, detectXcode and detectAndroidSDK return "" via stubs,
	// so DetectMacOSToolchains returns an empty slice.
	// On macOS, results depend on whether Xcode/Android SDK are installed.
	// Either way, returned tags must be valid.
	tools := DetectMacOSToolchains()
	for _, tool := range tools {
		if !ValidateTag(tool.Tag) {
			t.Errorf("invalid tag %q for tool %s", tool.Tag, tool.Name)
		}
		if tool.Version != "" {
			vTag := tool.Tag + ":" + tool.Version
			// Versioned tags with dots/spaces may not validate; just check the base tag
			t.Logf("tool %s: version=%q versioned_tag=%q valid=%v", tool.Name, tool.Version, vTag, ValidateTag(vTag))
		}
	}
}

func TestMacOSToolchainStruct(t *testing.T) {
	tc := MacOSToolchain{
		Name:    "xcode",
		Tag:     "tool:xcode",
		Path:    "/Applications/Xcode.app/Contents/Developer",
		Version: "15.4",
	}
	if tc.Name != "xcode" {
		t.Errorf("unexpected name: %s", tc.Name)
	}
	if tc.Tag != "tool:xcode" {
		t.Errorf("unexpected tag: %s", tc.Tag)
	}
	if !ValidateTag(tc.Tag) {
		t.Errorf("tool:xcode should be a valid tag")
	}
	// Validate that versioned tags work
	versionedTag := tc.Tag + ":" + tc.Version
	if !ValidateTag(versionedTag) {
		t.Errorf("tool:xcode:15.4 should be a valid tag, got invalid")
	}
}
