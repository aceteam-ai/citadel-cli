package status

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestPopulateCapabilityFlagsAlwaysSetsAllFour verifies that all four capability
// flags are non-nil after population, so every heartbeat emits concrete
// true/false values rather than omitting keys (citadel-cli#324).
func TestPopulateCapabilityFlagsAlwaysSetsAllFour(t *testing.T) {
	caps := &NodeCapabilities{}
	populateCapabilityFlags(caps, 0)

	if caps.Console == nil {
		t.Error("Console flag should be populated, got nil")
	}
	if caps.Desktop == nil {
		t.Error("Desktop flag should be populated, got nil")
	}
	if caps.Files == nil {
		t.Error("Files flag should be populated, got nil")
	}
	if caps.GPU == nil {
		t.Error("GPU flag should be populated, got nil")
	}
}

// TestCapabilityFlagsDesktopDerivedFromVNCPort verifies the desktop flag is true
// when a VNC port was already detected, without dialing.
func TestCapabilityFlagsDesktopDerivedFromVNCPort(t *testing.T) {
	caps := &NodeCapabilities{}
	populateCapabilityFlags(caps, 5900)
	if caps.Desktop == nil || !*caps.Desktop {
		t.Errorf("Desktop should be true when vncPort > 0, got %v", caps.Desktop)
	}
}

// TestCapabilityFlagsJSONContract verifies the wire format matches the backend
// contract exactly: keys console/desktop/files/gpu, boolean values, inside the
// capabilities object (aceteam#4223, PR #4231).
func TestCapabilityFlagsJSONContract(t *testing.T) {
	tr := true
	fa := false
	caps := &NodeCapabilities{
		Console: &tr,
		Desktop: &fa,
		Files:   &tr,
		GPU:     &fa,
	}
	b, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	got := string(b)

	for _, want := range []string{
		`"console":true`,
		`"desktop":false`,
		`"files":true`,
		`"gpu":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("capabilities JSON missing %q; got %s", want, got)
		}
	}
}

// TestNodeStatusCapabilitiesKeyPresent verifies the capabilities block is
// emitted under the "capabilities" key on NodeStatus (the heartbeat payload),
// matching CitadelStatus.capabilities on the backend.
func TestNodeStatusCapabilitiesKeyPresent(t *testing.T) {
	tr := true
	st := &NodeStatus{
		Version:      StatusVersion,
		Capabilities: &NodeCapabilities{Console: &tr},
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(b), `"capabilities":`) {
		t.Errorf("NodeStatus JSON missing capabilities key; got %s", string(b))
	}
}
