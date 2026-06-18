// internal/network/state_test.go
package network

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// TestResolveChownTarget_NotRoot verifies no chown when not running as root.
func TestResolveChownTarget_NotRoot(t *testing.T) {
	uid, gid, ok := resolveChownTargetWith(1000, "someuser", user.Lookup)
	if ok {
		t.Errorf("expected ok=false for non-root (uid=1000), got uid=%d gid=%d", uid, gid)
	}
}

// TestResolveChownTarget_RootWithSudoUser verifies chown target uses $SUDO_USER.
func TestResolveChownTarget_RootWithSudoUser(t *testing.T) {
	// Use a lookup function that returns a known user
	fakeLookup := func(username string) (*user.User, error) {
		if username == "citadel" {
			return &user.User{Uid: "1000", Gid: "1000", Username: "citadel"}, nil
		}
		if username == "testuser" {
			return &user.User{Uid: "1001", Gid: "1001", Username: "testuser"}, nil
		}
		return nil, fmt.Errorf("user not found: %s", username)
	}

	// Root with SUDO_USER set
	uid, gid, ok := resolveChownTargetWith(0, "testuser", fakeLookup)
	if !ok {
		t.Fatal("expected ok=true for root with SUDO_USER=testuser")
	}
	if uid != 1001 || gid != 1001 {
		t.Errorf("expected uid=1001 gid=1001, got uid=%d gid=%d", uid, gid)
	}
}

// TestResolveChownTarget_RootNoSudoUser verifies chown target defaults to "citadel".
func TestResolveChownTarget_RootNoSudoUser(t *testing.T) {
	fakeLookup := func(username string) (*user.User, error) {
		if username == "citadel" {
			return &user.User{Uid: "1000", Gid: "1000", Username: "citadel"}, nil
		}
		return nil, fmt.Errorf("user not found: %s", username)
	}

	// Root with no SUDO_USER (e.g., systemd running as root)
	uid, gid, ok := resolveChownTargetWith(0, "", fakeLookup)
	if !ok {
		t.Fatal("expected ok=true for root with empty SUDO_USER (falls back to citadel)")
	}
	if uid != 1000 || gid != 1000 {
		t.Errorf("expected uid=1000 gid=1000, got uid=%d gid=%d", uid, gid)
	}
}

// TestResolveChownTarget_RootUserNotFound verifies graceful handling when user doesn't exist.
func TestResolveChownTarget_RootUserNotFound(t *testing.T) {
	fakeLookup := func(username string) (*user.User, error) {
		return nil, fmt.Errorf("user not found: %s", username)
	}

	uid, gid, ok := resolveChownTargetWith(0, "nonexistent", fakeLookup)
	if ok {
		t.Errorf("expected ok=false when user lookup fails, got uid=%d gid=%d", uid, gid)
	}
}

// TestResolveChownTarget_RootBadUID verifies graceful handling of non-numeric UID.
func TestResolveChownTarget_RootBadUID(t *testing.T) {
	fakeLookup := func(username string) (*user.User, error) {
		return &user.User{Uid: "notanumber", Gid: "1000", Username: username}, nil
	}

	_, _, ok := resolveChownTargetWith(0, "citadel", fakeLookup)
	if ok {
		t.Error("expected ok=false when UID is not numeric")
	}
}

// TestResolveChownTarget_RootBadGID verifies graceful handling of non-numeric GID.
func TestResolveChownTarget_RootBadGID(t *testing.T) {
	fakeLookup := func(username string) (*user.User, error) {
		return &user.User{Uid: "1000", Gid: "notanumber", Username: username}, nil
	}

	_, _, ok := resolveChownTargetWith(0, "citadel", fakeLookup)
	if ok {
		t.Error("expected ok=false when GID is not numeric")
	}
}

// TestChownRecursive_CreatesCorrectPermissions verifies the recursive chown walks all files.
// This test can only verify the walk logic since os.Chown requires root for real ownership changes.
func TestChownRecursive_CreatesCorrectPermissions(t *testing.T) {
	// Create a temp directory tree
	tmpDir, err := os.MkdirTemp("", "citadel-chown-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create nested structure mimicking the real state dir
	networkDir := filepath.Join(tmpDir, "network")
	if err := os.MkdirAll(networkDir, 0700); err != nil {
		t.Fatalf("Failed to create network dir: %v", err)
	}

	stateFile := filepath.Join(networkDir, "tailscaled.state")
	if err := os.WriteFile(stateFile, []byte("fake-state"), 0600); err != nil {
		t.Fatalf("Failed to create state file: %v", err)
	}

	machineKey := filepath.Join(networkDir, "machine-key")
	if err := os.WriteFile(machineKey, []byte("fake-key"), 0600); err != nil {
		t.Fatalf("Failed to create machine-key file: %v", err)
	}

	// chownRecursive with current user's UID/GID (should succeed without root)
	currentUID := os.Getuid()
	currentGID := os.Getgid()
	chownRecursive(tmpDir, currentUID, currentGID)

	// Verify all files still exist and are accessible
	entries, err := os.ReadDir(networkDir)
	if err != nil {
		t.Fatalf("Failed to read network dir after chown: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("Expected 2 files in network dir, got %d", len(entries))
	}

	// Verify files are readable
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Errorf("State file not readable after chown: %v", err)
	}
	if string(data) != "fake-state" {
		t.Errorf("State file content changed: %q", data)
	}
}

// TestEnsureStateDirCreatesDirectory verifies EnsureStateDir creates the directory.
func TestEnsureStateDirCreatesDirectory(t *testing.T) {
	// This test exercises the real EnsureStateDir. Since we're not root,
	// fixStateDirOwnership will be a no-op, but the directory creation
	// should still work.
	stateDir := GetStateDir()

	// If the state dir doesn't exist, EnsureStateDir should create it
	dir, err := EnsureStateDir()
	if err != nil {
		t.Fatalf("EnsureStateDir() error: %v", err)
	}
	if dir != stateDir {
		t.Errorf("EnsureStateDir() returned %q, want %q", dir, stateDir)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("State dir not accessible after EnsureStateDir: %v", err)
	}
	if !info.IsDir() {
		t.Error("State dir path is not a directory")
	}
}

// TestIsSystemProfilePath verifies detection of SYSTEM profile paths on Windows.
func TestIsSystemProfilePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{`C:\Windows\system32\config\systemprofile\citadel-node`, true},
		{`C:\WINDOWS\System32\config\SystemProfile\data`, true},
		{`C:\Users\acewin\citadel-node`, false},
		{`C:\Users\acewin\AppData\Local\citadel-node`, false},
		{`/home/citadel/citadel-node`, false},
	}
	for _, tt := range tests {
		got := isSystemProfilePath(tt.path)
		if got != tt.want {
			t.Errorf("isSystemProfilePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// TestFixStatePermissions_NoOpWhenNotRoot verifies FixStatePermissions is safe to call as non-root.
func TestFixStatePermissions_NoOpWhenNotRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("Test only meaningful when not running as root")
	}

	// Should not panic or error
	FixStatePermissions()
}
