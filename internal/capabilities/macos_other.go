//go:build !darwin

package capabilities

// detectXcode is a no-op on non-macOS platforms.
func detectXcode() string {
	return ""
}

// detectXcodeVersion is a no-op on non-macOS platforms.
func detectXcodeVersion() string {
	return ""
}

// detectAndroidSDK is a no-op on non-macOS platforms.
func detectAndroidSDK() string {
	return ""
}
