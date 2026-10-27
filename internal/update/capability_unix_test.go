//go:build linux || darwin

package update

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelfUpdateCapabilityProbePreservesInstalledFileAndCreatesNoLock(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	before := []byte("synthetic installed bytes")
	if err := os.WriteFile(destination, before, 0755); err != nil {
		t.Fatal(err)
	}
	identity, err := statInstalledIdentity(destination)
	if err != nil {
		t.Fatal(err)
	}
	capability := inspectSelfUpdatePath(destination)
	if capability.State != SelfUpdateAvailable || capability.Directory != dir || capability.BinaryOwner == "" || capability.WorkerUser == "" {
		t.Fatalf("safe fixture install not observed as writable: %+v", capability)
	}
	if after, err := os.ReadFile(destination); err != nil || !bytes.Equal(before, after) {
		t.Fatal("capability probe changed the installed bytes")
	}
	if after, err := statInstalledIdentity(destination); err != nil || after != identity {
		t.Fatal("capability probe changed the installed owner/group")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "citadel" {
		t.Fatalf("probe left a stage or provisioned a lock: %v %v", entries, err)
	}
}

func TestSelfUpdateCapabilityRefusesUnwritableInstallDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create a file in a mode-0555 directory; this fixture requires an unprivileged process")
	}
	isolateState(t)
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	if err := os.WriteFile(destination, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	capability := inspectSelfUpdatePath(destination)
	if capability.State != SelfUpdatePrivileged || capability.Directory != dir {
		t.Fatalf("unwritable install was admitted: %+v", capability)
	}
	if !strings.Contains(capability.Message(), capability.BinaryOwner) || !strings.Contains(capability.Message(), capability.WorkerUser) || !strings.Contains(capability.Message(), "privileged installer") {
		t.Fatalf("refusal omitted identities/remediation: %s", capability.Message())
	}
}

func TestSelfUpdateCapabilityRefusesUnsafeExistingLock(t *testing.T) {
	isolateState(t)
	dir := t.TempDir()
	destination := filepath.Join(dir, "citadel")
	if err := os.WriteFile(destination, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination+".citadel-update.lock", []byte("fixture"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(destination+".citadel-update.lock", 0666); err != nil {
		t.Fatal(err)
	}
	if c := inspectSelfUpdatePath(destination); c.State != SelfUpdateUnknown {
		t.Fatalf("unsafe lock became install authority: %+v", c)
	}
}

func TestFirstLockOwnerRefusalNamesBothIdentitiesWithoutCreatingLock(t *testing.T) {
	isolateState(t)
	path := filepath.Join(t.TempDir(), "fixture.lock")
	foreignUID := uint32(os.Geteuid()) + 1
	file, err := openMutationLock(path, installedIdentity{uid: foreignUID}, true)
	if file != nil {
		_ = file.Close()
	}
	if err == nil || !strings.Contains(err.Error(), unixUserLabel(foreignUID)) || !strings.Contains(err.Error(), unixUserLabel(uint32(os.Geteuid()))) || !strings.Contains(err.Error(), "run the installer as the binary owner") {
		t.Fatalf("owner refusal not actionable: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("foreign-owner refusal created a lock")
	}
}
