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
	// Create a temp file with content shorter than 8 chars
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "machine-id")
	if err := os.WriteFile(tmpFile, []byte("abc\n"), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	// We can't easily test getShortMachineID with a custom path since it
	// reads /etc/machine-id directly. This test documents the expected
	// behavior: strings shorter than 8 chars should cause an error.
	t.Log("getShortMachineID returns error for machine-id shorter than 8 chars")
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
