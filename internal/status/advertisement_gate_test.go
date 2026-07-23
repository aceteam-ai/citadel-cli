package status

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAdvertisement_DisabledSurfacesNotAdvertised is the White Whale fix
// (aceteam#6524): even when the node is physically capable, the heartbeat
// capability flags for console/desktop/files must read FALSE while the operator
// has those permissions disabled — so the Fabric web console does not present a
// live terminal/screen/file browser for a freshly joined node. GPU is not gated.
func TestAdvertisement_DisabledSurfacesNotAdvertised(t *testing.T) {
	dir := t.TempDir()
	// Point ConfigDir and the workspace resolver at the temp HOME. The config.yaml
	// marker makes the root code path of resolveConfigDir also resolve here.
	t.Setenv("HOME", dir)
	t.Setenv("SUDO_USER", "")
	t.Setenv("CITADEL_WORKSPACE", "")
	cfgDir := filepath.Join(dir, ".citadel-cli")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("node:\n  name: t\n"), 0600); err != nil {
		t.Fatalf("write config marker: %v", err)
	}
	// Make the files hardware-signal TRUE by creating the workspace, so the gate
	// (not the absence of a workspace) is what suppresses the files flag.
	if err := os.MkdirAll(filepath.Join(dir, "citadel-node", "workspace"), 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	// permissions.yaml with the sensitive surfaces DISABLED.
	perms := []byte("console: false\ndesktop: false\nfiles: false\nservices: true\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "permissions.yaml"), perms, 0600); err != nil {
		t.Fatalf("write perms: %v", err)
	}

	caps := &NodeCapabilities{}
	// vncPort > 0 would otherwise make desktop capable; pass a live port to prove
	// the permission gate — not the absence of hardware — is what suppresses it.
	populateCapabilityFlags(caps, 5901)

	if caps.Console == nil || *caps.Console {
		t.Errorf("console must not advertise while disabled, got %v", caps.Console)
	}
	if caps.Desktop == nil || *caps.Desktop {
		t.Errorf("desktop must not advertise while disabled, got %v", caps.Desktop)
	}
	if caps.Files == nil || *caps.Files {
		t.Errorf("files must not advertise while disabled (workspace present), got %v", caps.Files)
	}
}

// TestAdvertisement_EnabledFilesAdvertised confirms opting in restores the
// advertisement: with files enabled AND a workspace present, the files flag is
// true. (Files is the deterministic one — its hardware signal is a dir stat.)
func TestAdvertisement_EnabledFilesAdvertised(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("SUDO_USER", "")
	t.Setenv("CITADEL_WORKSPACE", "")
	cfgDir := filepath.Join(dir, ".citadel-cli")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatalf("mkdir cfg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("node:\n  name: t\n"), 0600); err != nil {
		t.Fatalf("write config marker: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "citadel-node", "workspace"), 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	perms := []byte("files: true\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "permissions.yaml"), perms, 0600); err != nil {
		t.Fatalf("write perms: %v", err)
	}

	caps := &NodeCapabilities{}
	populateCapabilityFlags(caps, 0)

	if caps.Files == nil || !*caps.Files {
		t.Errorf("files should advertise when enabled with a workspace present, got %v", caps.Files)
	}
}
