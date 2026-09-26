package status

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
)

// TestAdvertisement_DisabledSurfacesNotAdvertised is the White Whale fix
// (aceteam#6524): even when the node is physically capable, the heartbeat
// capability flags for console/desktop/files must read FALSE while the operator
// has those permissions disabled — so the Fabric web console does not present a
// live terminal/screen/file browser for a freshly joined node. GPU is not gated.
func TestAdvertisement_DisabledSurfacesNotAdvertised(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	t.Setenv("CITADEL_WORKSPACE", workspace)
	// Make the files hardware-signal TRUE by creating the workspace, so the gate
	// (not the absence of a workspace) is what suppresses the files flag.
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	// Use an injected node policy; the hardware probes below are independent of
	// the invoker's configuration directory.
	perms := config.DefaultPermissions()

	caps := &NodeCapabilities{}
	// vncPort > 0 would otherwise make desktop capable; pass a live port to prove
	// the permission gate — not the absence of hardware — is what suppresses it.
	populateCapabilityFlags(caps, 5901, perms)

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
	t.Setenv("CITADEL_WORKSPACE", filepath.Join(dir, "workspace"))
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0755); err != nil {
		t.Fatalf("mkdir configured workspace: %v", err)
	}
	perms := config.DefaultPermissions()
	perms.Files = true

	caps := &NodeCapabilities{}
	populateCapabilityFlags(caps, 0, perms)

	if caps.Files == nil || !*caps.Files {
		t.Errorf("files should advertise when enabled with a workspace present, got %v", caps.Files)
	}
}
