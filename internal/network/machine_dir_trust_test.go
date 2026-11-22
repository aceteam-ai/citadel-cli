package network

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckMachineConfigDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "Citadel")
	called := 0
	acl := func(string) error { called++; return errors.New("owned by a non-administrator") }

	// Missing: nothing is read from it, so it is trusted without an ACL read.
	if err := checkMachineConfigDir(dir, acl); err != nil || called != 0 {
		t.Fatalf("missing dir: %v (acl calls %d)", err, called)
	}
	// Present: the ACL decides.
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkMachineConfigDir(dir, acl); err == nil || called != 1 {
		t.Fatalf("untrusted ACL: %v (acl calls %d)", err, called)
	}
	if err := checkMachineConfigDir(dir, func(string) error { return nil }); err != nil {
		t.Fatalf("trusted ACL: %v", err)
	}
	// A file or a link in its place is never trusted.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkMachineConfigDir(file, nil); err == nil {
		t.Fatal("a file was trusted")
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err == nil {
		if err := checkMachineConfigDir(link, func(string) error { return nil }); err == nil {
			t.Fatal("a symlink was trusted")
		}
	}
}

// TestCheckMachineConfigDirTrustedUsesTheScopedMachineDir: the exported check
// reads the same machine dir the pointer and global config come from.
func TestCheckMachineConfigDirTrustedUsesTheScopedMachineDir(t *testing.T) {
	node, machine := t.TempDir(), t.TempDir()
	restore, err := OverrideStateDirectoriesForTesting(node, machine)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restore(); err != nil {
			t.Error(err)
		}
	}()
	if got := getGlobalConfigDirForState(); got != machine {
		t.Fatalf("machine dir: %s", got)
	}
	if checkMachineDirACL == nil {
		if err := CheckMachineConfigDirTrusted(); err != nil {
			t.Fatalf("plain machine dir off Windows: %v", err)
		}
	}
}
