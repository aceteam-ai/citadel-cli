// internal/platform/machineid_test.go
package platform

import (
	"encoding/hex"
	"testing"
)

func TestGenerateMachineID(t *testing.T) {
	// Test that GenerateMachineID returns a non-empty string
	id, err := GenerateMachineID()
	if err != nil {
		t.Fatalf("GenerateMachineID failed: %v", err)
	}

	if id == "" {
		t.Error("GenerateMachineID returned empty string")
	}

	// Verify it's a valid SHA-256 hex string (64 characters)
	if len(id) != 64 {
		t.Errorf("Expected 64-character hex string, got %d characters", len(id))
	}

	// Verify it's valid hex
	_, err = hex.DecodeString(id)
	if err != nil {
		t.Errorf("GenerateMachineID returned invalid hex: %v", err)
	}
}

func TestGenerateMachineIDConsistency(t *testing.T) {
	// Test that multiple calls return the same ID (stability)
	id1, err := GenerateMachineID()
	if err != nil {
		t.Fatalf("First GenerateMachineID call failed: %v", err)
	}

	id2, err := GenerateMachineID()
	if err != nil {
		t.Fatalf("Second GenerateMachineID call failed: %v", err)
	}

	if id1 != id2 {
		t.Errorf("GenerateMachineID returned different values: %s vs %s", id1, id2)
	}
}

func TestGetPrimaryMAC(t *testing.T) {
	// Test getPrimaryMAC returns a value (may be empty on some CI systems)
	mac, err := getPrimaryMAC()
	// Don't fail on error - some systems may not have suitable interfaces
	if err == nil {
		if mac == "" {
			t.Error("getPrimaryMAC returned empty string without error")
		}
		// MAC address format: XX:XX:XX:XX:XX:XX
		if len(mac) != 17 {
			t.Logf("MAC address has unexpected length: %s", mac)
		}
	} else {
		t.Logf("getPrimaryMAC returned error (may be expected in CI): %v", err)
	}
}

func TestGetMachineUUID(t *testing.T) {
	// Test getMachineUUID (platform-specific)
	uuid, err := getMachineUUID()
	// Don't fail on error - may not be available in all environments
	if err == nil {
		if uuid == "" {
			t.Error("getMachineUUID returned empty string without error")
		}
		t.Logf("Machine UUID: %s", uuid)
	} else {
		t.Logf("getMachineUUID returned error (may be expected in some environments): %v", err)
	}
}
