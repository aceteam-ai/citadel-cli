package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPopulateCapabilityFlagsAlwaysSetsAll verifies that every capability flag
// is non-nil after population, so every heartbeat emits concrete true/false
// values rather than omitting keys (citadel-cli#324, plus h264 in #338).
func TestPopulateCapabilityFlagsAlwaysSetsAll(t *testing.T) {
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
	if caps.H264 == nil {
		t.Error("H264 flag should be populated, got nil")
	}
}

// writeDesktopPerm points ConfigDir at a temp HOME carrying a permissions.yaml
// with the given desktop value, and returns nothing (the env is restored by
// t.Setenv). The config.yaml marker makes the root code path of resolveConfigDir
// also resolve here. Used to exercise the aceteam#6524 gate deterministically.
func writeDesktopPerm(t *testing.T, desktopEnabled bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("SUDO_USER", "")
	cfgDir := filepath.Join(dir, ".citadel-cli")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("node:\n  name: t\n"), 0600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	perms := "desktop: false\n"
	if desktopEnabled {
		perms = "desktop: true\n"
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "permissions.yaml"), []byte(perms), 0600); err != nil {
		t.Fatalf("write perms: %v", err)
	}
}

// TestCapabilityFlagsDesktopDerivedFromVNCPort verifies the desktop flag under
// the aceteam#6524 opt-in posture: a detected VNC port advertises desktop ONLY
// when the operator has enabled the `desktop` permission. The VNC port is the
// hardware signal; the permission is the consent gate — both must hold. This
// replaces the old assertion that a VNC port alone advertised desktop (which put
// a node's screen on the mesh before the operator opted in — the White Whale
// landmine).
func TestCapabilityFlagsDesktopDerivedFromVNCPort(t *testing.T) {
	t.Run("enabled: vnc port advertises desktop", func(t *testing.T) {
		writeDesktopPerm(t, true)
		caps := &NodeCapabilities{}
		populateCapabilityFlags(caps, 5900)
		if caps.Desktop == nil || !*caps.Desktop {
			t.Errorf("Desktop should be true when vncPort > 0 AND desktop is enabled, got %v", caps.Desktop)
		}
	})

	t.Run("disabled: vnc port does NOT advertise desktop", func(t *testing.T) {
		writeDesktopPerm(t, false)
		caps := &NodeCapabilities{}
		populateCapabilityFlags(caps, 5900)
		if caps.Desktop == nil || *caps.Desktop {
			t.Errorf("Desktop must stay false when desktop is disabled, even with vncPort > 0, got %v", caps.Desktop)
		}
	})
}

// TestCapabilityFlagsJSONContract verifies the wire format matches the backend
// contract exactly: keys console/desktop/files/gpu/h264, boolean values, inside
// the capabilities object (aceteam#4223, PR #4231; h264 in citadel-cli#338).
func TestCapabilityFlagsJSONContract(t *testing.T) {
	tr := true
	fa := false
	caps := &NodeCapabilities{
		Console: &tr,
		Desktop: &fa,
		Files:   &tr,
		GPU:     &fa,
		H264:    &tr,
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
		`"h264":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("capabilities JSON missing %q; got %s", want, got)
		}
	}
}

// TestPopulateServicesAdvertisesRunnableSet verifies that AvailableServices is
// populated with the serving services this build can deploy (embedded
// ServiceMap keys) so the fabric can schedule engine-specific deploys to
// capable nodes (aceteam#4483). Asserts concrete known keys rather than
// comparing to GetAvailableServices() (which would be circular).
func TestPopulateServicesAdvertisesRunnableSet(t *testing.T) {
	caps := &NodeCapabilities{}
	populateServices(caps)

	if len(caps.AvailableServices) == 0 {
		t.Fatal("AvailableServices should be populated, got empty")
	}

	set := make(map[string]bool, len(caps.AvailableServices))
	for _, s := range caps.AvailableServices {
		set[s] = true
	}
	for _, want := range []string{"vllm", "ollama", "diffusers"} {
		if !set[want] {
			t.Errorf("AvailableServices missing %q; got %v", want, caps.AvailableServices)
		}
	}

	// Sorted output keeps heartbeats deterministic.
	for i := 1; i < len(caps.AvailableServices); i++ {
		if caps.AvailableServices[i-1] > caps.AvailableServices[i] {
			t.Errorf("AvailableServices not sorted: %v", caps.AvailableServices)
			break
		}
	}
}

// TestAvailableServicesJSONContract verifies the wire format: an
// "available_services" array nested inside the capabilities object, distinct
// from the top-level NodeStatus.Services (aceteam#4483).
func TestAvailableServicesJSONContract(t *testing.T) {
	caps := &NodeCapabilities{
		AvailableServices: []string{"diffusers", "ollama", "vllm"},
	}
	b, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"available_services":["diffusers","ollama","vllm"]`) {
		t.Errorf("capabilities JSON missing available_services array; got %s", got)
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
