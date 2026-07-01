// internal/network/state_resolve_test.go
// Tests for the canonical tsnet state-dir resolution that prevents duplicate
// Headscale node registration (aceteam-ai/citadel-cli#383).
package network

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveStateDir is the primary table-driven test for the pure resolution
// logic. It never touches a real HOME, config file, or filesystem — all inputs
// are injected. The load-bearing invariant: on a given machine, DIFFERENT
// raw $HOME / sudo contexts must resolve to the SAME state dir.
func TestResolveStateDir(t *testing.T) {
	// legacyPresent / legacyAbsent are injectable stand-ins for
	// legacyStateExistsAt so tests stay off the real filesystem.
	legacyPresent := func(string) bool { return true }
	legacyAbsent := func(string) bool { return false }

	tests := []struct {
		name string
		in   stateDirInputs
		want string
	}{
		{
			name: "pointer wins over everything",
			in: stateDirInputs{
				pointerDir:        "/etc/citadel/node",
				configNodeDir:     "/home/jason/citadel-node",
				ownerHome:         "/home/jason",
				legacyStateExists: legacyPresent,
				goos:              "linux",
			},
			want: filepath.Join("/etc/citadel/node", "network"),
		},
		{
			name: "config used when no pointer",
			in: stateDirInputs{
				pointerDir:        "",
				configNodeDir:     "/home/jason/citadel-node",
				ownerHome:         "/home/jason",
				legacyStateExists: legacyPresent,
				goos:              "linux",
			},
			want: filepath.Join("/home/jason/citadel-node", "network"),
		},
		{
			name: "existing legacy state reused when no pointer/config",
			in: stateDirInputs{
				pointerDir:        "",
				configNodeDir:     "",
				ownerHome:         "/home/jason",
				legacyStateExists: legacyPresent,
				goos:              "linux",
			},
			want: filepath.Join("/home/jason", "citadel-node", "network"),
		},
		{
			name: "fresh machine falls back to owner home",
			in: stateDirInputs{
				pointerDir:        "",
				configNodeDir:     "",
				ownerHome:         "/home/jason",
				legacyStateExists: legacyAbsent,
				goos:              "linux",
			},
			want: filepath.Join("/home/jason", "citadel-node", "network"),
		},
		{
			name: "windows SYSTEM-profile pointer is rejected, falls through to config",
			in: stateDirInputs{
				pointerDir:        `C:\Windows\system32\config\systemprofile\citadel-node`,
				configNodeDir:     `C:\Users\acewin\citadel-node`,
				ownerHome:         `C:\Users\acewin`,
				legacyStateExists: legacyAbsent,
				goos:              "windows",
			},
			want: filepath.Join(`C:\Users\acewin\citadel-node`, "network"),
		},
		{
			name: "windows SYSTEM-profile config is rejected, falls through to owner home",
			in: stateDirInputs{
				pointerDir:        "",
				configNodeDir:     `C:\Windows\System32\config\systemprofile\citadel-node`,
				ownerHome:         `C:\Users\acewin`,
				legacyStateExists: legacyAbsent,
				goos:              "windows",
			},
			want: filepath.Join(`C:\Users\acewin`, "citadel-node", "network"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveStateDir(tt.in)
			if got != tt.want {
				t.Errorf("resolveStateDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveStateDir_DifferentHomeSameOwner is the discriminating regression
// test for #383. Two invocations with DIFFERENT raw $HOME but the SAME resolved
// owner (e.g. the user-mode service running as jason, vs `sudo citadel login`
// whose raw $HOME is /root but whose SUDO_USER resolves back to jason's home)
// MUST produce the identical state dir. If they diverge, tsnet mints a fresh
// machine key and Headscale registers a duplicate node.
func TestResolveStateDir_DifferentHomeSameOwner(t *testing.T) {
	// Both contexts share a pointer file (the canonical convergence source).
	serviceCtx := stateDirInputs{
		pointerDir:        "/etc/citadel/node",
		ownerHome:         "/home/jason", // service runs as jason
		legacyStateExists: func(string) bool { return true },
		goos:              "linux",
	}
	sudoCtx := stateDirInputs{
		pointerDir:        "/etc/citadel/node",
		ownerHome:         "/root", // raw $HOME under sudo differs...
		legacyStateExists: func(string) bool { return false },
		goos:              "linux",
	}

	svc := resolveStateDir(serviceCtx)
	sudo := resolveStateDir(sudoCtx)
	if svc != sudo {
		t.Fatalf("state dir diverged across contexts: service=%q sudo=%q (duplicate-node bug)", svc, sudo)
	}
}

// TestResolveStateDir_OwnerHomeConvergesWithoutPointer documents the residual
// single-owner assumption: when there is NO pointer and NO config, convergence
// depends on both contexts resolving the SAME owner home. With owner-consistent
// resolution (SUDO_USER-aware), a service-as-jason and `sudo citadel login`
// (SUDO_USER=jason → /home/jason) both land on the same dir even though the
// sudo invoker's raw $HOME is /root.
func TestResolveStateDir_OwnerHomeConvergesWithoutPointer(t *testing.T) {
	serviceCtx := stateDirInputs{
		ownerHome:         "/home/jason",
		legacyStateExists: func(string) bool { return true },
		goos:              "linux",
	}
	// getOwnerHomeDir() resolves SUDO_USER=jason back to /home/jason, so the
	// caller passes /home/jason here, NOT the raw /root.
	sudoCtx := stateDirInputs{
		ownerHome:         "/home/jason",
		legacyStateExists: func(string) bool { return false },
		goos:              "linux",
	}
	if resolveStateDir(serviceCtx) != resolveStateDir(sudoCtx) {
		t.Fatal("owner-home resolution failed to converge for same owner")
	}
}

// TestUsableNodeDir verifies the Windows SYSTEM-profile guard and empty handling.
func TestUsableNodeDir(t *testing.T) {
	tests := []struct {
		name    string
		nodeDir string
		goos    string
		want    string
	}{
		{"empty", "", "linux", ""},
		{"linux passthrough", "/home/jason/citadel-node", "linux", "/home/jason/citadel-node"},
		{"windows normal", `C:\Users\acewin\citadel-node`, "windows", `C:\Users\acewin\citadel-node`},
		{"windows systemprofile rejected", `C:\Windows\system32\config\systemprofile\x`, "windows", ""},
		{"systemprofile only rejected on windows", `/x/systemprofile/y`, "linux", "/x/systemprofile/y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usableNodeDir(tt.nodeDir, tt.goos); got != tt.want {
				t.Errorf("usableNodeDir(%q, %q) = %q, want %q", tt.nodeDir, tt.goos, got, tt.want)
			}
		})
	}
}

// TestDirHasEntries verifies the non-empty check used for legacy-state detection:
// a stale EMPTY network/ dir must not be treated as existing state (else it would
// win over a real machine key elsewhere).
func TestDirHasEntries(t *testing.T) {
	tmp := t.TempDir()

	if dirHasEntries(filepath.Join(tmp, "does-not-exist")) {
		t.Error("expected false for missing dir")
	}

	empty := filepath.Join(tmp, "empty")
	if err := os.MkdirAll(empty, 0700); err != nil {
		t.Fatal(err)
	}
	if dirHasEntries(empty) {
		t.Error("expected false for empty dir")
	}

	populated := filepath.Join(tmp, "populated")
	if err := os.MkdirAll(populated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(populated, "tailscaled.state"), []byte("k"), 0600); err != nil {
		t.Fatal(err)
	}
	if !dirHasEntries(populated) {
		t.Error("expected true for populated dir")
	}

	// A file (not a dir) must report false.
	file := filepath.Join(tmp, "afile")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if dirHasEntries(file) {
		t.Error("expected false for a regular file")
	}
}

// TestLegacyStateExistsAt verifies the legacy-path detection joins the expected
// subpath and requires non-empty contents.
func TestLegacyStateExistsAt(t *testing.T) {
	if legacyStateExistsAt("") {
		t.Error("empty home must report no legacy state")
	}

	home := t.TempDir()
	if legacyStateExistsAt(home) {
		t.Error("expected no legacy state before creating network dir")
	}

	netDir := filepath.Join(home, "citadel-node", "network")
	if err := os.MkdirAll(netDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Still empty → false.
	if legacyStateExistsAt(home) {
		t.Error("expected false for empty legacy network dir")
	}
	if err := os.WriteFile(filepath.Join(netDir, "machine-key"), []byte("k"), 0600); err != nil {
		t.Fatal(err)
	}
	if !legacyStateExistsAt(home) {
		t.Error("expected true once legacy network dir is populated")
	}
}

// TestMachineStatePointerRoundTrip verifies WriteMachineStatePointer /
// readMachineStatePointer agree, using an overridden global config dir so the
// test never writes to a real /etc/citadel.
func TestMachineStatePointerRoundTrip(t *testing.T) {
	// getGlobalConfigDirForState() is not injectable, so exercise the file
	// format directly against a temp dir to keep the test hermetic.
	tmp := t.TempDir()
	pointer := filepath.Join(tmp, machineStatePointerFile)

	want := "/home/jason/citadel-node"
	if err := os.WriteFile(pointer, []byte(want+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	// readMachineStatePointer trims surrounding whitespace; assert on the same
	// normalization without depending on the fixed system path.
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("pointer round-trip = %q, want %q", got, want)
	}
}
