package protocol

import "testing"

// TestFabricProtocolVersionIsPositive guards against the constant accidentally
// being zeroed or made negative — the wire contract starts at v1.
func TestFabricProtocolVersionIsPositive(t *testing.T) {
	if FabricProtocolVersion < 1 {
		t.Fatalf("FabricProtocolVersion must be >= 1, got %d", FabricProtocolVersion)
	}
}
