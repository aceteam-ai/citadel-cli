package platform

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateCitadelHostname(t *testing.T) {
	hostname, err := GenerateCitadelHostname()
	if err != nil {
		t.Fatalf("GenerateCitadelHostname() failed: %v", err)
	}

	// Must have the citadel- prefix
	if !strings.HasPrefix(hostname, "citadel-") {
		t.Errorf("hostname %q does not have citadel- prefix", hostname)
	}

	// Total length should be "citadel-" (8) + short_id (8) = 16
	if len(hostname) != 16 {
		t.Errorf("hostname %q has length %d, want 16", hostname, len(hostname))
	}

	// The suffix should be valid lowercase hex
	suffix := hostname[len("citadel-"):]
	if _, err := hex.DecodeString(suffix); err != nil {
		t.Errorf("hostname suffix %q is not valid hex: %v", suffix, err)
	}
}

func TestGenerateCitadelHostnameConsistency(t *testing.T) {
	// On systems with /etc/machine-id, the hostname should be consistent
	if _, err := os.ReadFile("/etc/machine-id"); err != nil {
		t.Skip("no /etc/machine-id on this system, skipping consistency test")
	}

	h1, err := GenerateCitadelHostname()
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	h2, err := GenerateCitadelHostname()
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	if h1 != h2 {
		t.Errorf("hostname not consistent across calls: %q vs %q", h1, h2)
	}
}

func TestGetShortMachineID(t *testing.T) {
	id, err := getShortMachineID()
	if err != nil {
		t.Skipf("cannot read /etc/machine-id: %v", err)
	}

	if len(id) != 8 {
		t.Errorf("short machine ID length = %d, want 8", len(id))
	}

	// Should be valid hex
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("short machine ID %q is not valid hex: %v", id, err)
	}
}

func TestGetShortMachineIDTooShort(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "machine-id")
	if err := os.WriteFile(tmpFile, []byte("abc\n"), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	// Override the machine-id path to test the too-short branch
	old := machineIDPath
	machineIDPath = tmpFile
	defer func() { machineIDPath = old }()

	_, err := getShortMachineID()
	if err == nil {
		t.Error("expected error for machine-id shorter than 8 chars, got nil")
	}
}

func TestGetShortMachineIDMissing(t *testing.T) {
	// Override to a non-existent path
	old := machineIDPath
	machineIDPath = "/tmp/does-not-exist-citadel-test"
	defer func() { machineIDPath = old }()

	_, err := getShortMachineID()
	if err == nil {
		t.Error("expected error for missing machine-id file, got nil")
	}
}

func TestGenerateCitadelHostnameFallbackToRandom(t *testing.T) {
	// Point machine-id to a non-existent file to force random fallback
	old := machineIDPath
	machineIDPath = "/tmp/does-not-exist-citadel-test"
	defer func() { machineIDPath = old }()

	hostname, err := GenerateCitadelHostname()
	if err != nil {
		t.Fatalf("GenerateCitadelHostname() with missing machine-id failed: %v", err)
	}

	if !strings.HasPrefix(hostname, "citadel-") {
		t.Errorf("hostname %q does not have citadel- prefix", hostname)
	}

	if len(hostname) != 16 {
		t.Errorf("hostname %q has length %d, want 16", hostname, len(hostname))
	}

	// Random fallback should produce different values each time
	hostname2, _ := GenerateCitadelHostname()
	if hostname == hostname2 {
		t.Errorf("random fallback produced same hostname twice: %q", hostname)
	}
}

func TestGenerateCitadelHostnameWithCustomMachineID(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "machine-id")
	if err := os.WriteFile(tmpFile, []byte("deadbeef12345678abcdef\n"), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	old := machineIDPath
	machineIDPath = tmpFile
	defer func() { machineIDPath = old }()

	hostname, err := GenerateCitadelHostname()
	if err != nil {
		t.Fatalf("GenerateCitadelHostname() failed: %v", err)
	}

	expected := "citadel-deadbeef"
	if hostname != expected {
		t.Errorf("hostname = %q, want %q", hostname, expected)
	}
}

func TestGenerateRandomHexID(t *testing.T) {
	id, err := generateRandomHexID(8)
	if err != nil {
		t.Fatalf("generateRandomHexID(8) failed: %v", err)
	}

	if len(id) != 8 {
		t.Errorf("random hex ID length = %d, want 8", len(id))
	}

	// Should be valid hex
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("random hex ID %q is not valid hex: %v", id, err)
	}
}

func TestGenerateRandomHexIDUniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := generateRandomHexID(8)
		if err != nil {
			t.Fatalf("generateRandomHexID failed on iteration %d: %v", i, err)
		}
		if ids[id] {
			t.Errorf("duplicate random ID %q on iteration %d", id, i)
		}
		ids[id] = true
	}
}

func TestIsGenericHostname(t *testing.T) {
	generic := []string{
		"", "localhost", "debian", "ubuntu", "fedora",
		"archlinux", "nixos", "raspberrypi", "linux",
		"host", "changeme", "default",
		"DEBIAN", "Ubuntu", " debian ", // case/whitespace variants
	}
	for _, name := range generic {
		if !IsGenericHostname(name) {
			t.Errorf("IsGenericHostname(%q) = false, want true", name)
		}
	}

	meaningful := []string{
		"aceteamvm", "gpu-server", "jason-desktop",
		"citadel-23bd5f21", "my-workstation", "prod-node-1",
	}
	for _, name := range meaningful {
		if IsGenericHostname(name) {
			t.Errorf("IsGenericHostname(%q) = true, want false", name)
		}
	}
}

func TestSetHostnameNonLinux(t *testing.T) {
	// SetHostname is a no-op on non-Linux platforms
	if IsLinux() {
		t.Skip("skipping non-Linux test on Linux")
	}

	// Should return nil without doing anything
	if err := SetHostname("test-hostname"); err != nil {
		t.Errorf("SetHostname on non-Linux returned error: %v", err)
	}
}
