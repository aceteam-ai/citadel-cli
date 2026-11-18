//go:build windows

package localcobrowse

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"golang.org/x/sys/windows"
)

func TestUploadsCachedFileBecomingUnreadableDACL(t *testing.T) {
	dir, s := uploadFixture(t)
	if !s.Status().Enabled {
		t.Fatal("fixture")
	}
	path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
	before, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, before) {
		t.Fatalf("fixture metadata: %v", err)
	}
	original, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := original.DACL()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			nil, nil, acl, nil); err != nil {
			t.Error("restore fixture DACL:", err)
		}
	})
	// An empty protected DACL keeps ownership and privacy unchanged, but
	// revokes data-read access without changing file identity, size or mtime.
	denied, err := windows.SecurityDescriptorFromString("D:P")
	if err != nil {
		t.Fatal(err)
	}
	empty, _, err := denied.DACL()
	if err != nil || empty == nil || empty.AceCount != 0 {
		t.Fatalf("empty DACL fixture: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, empty, nil); err != nil {
		t.Fatal(err)
	}
	if err := platform.ValidatePrivateFile(path); err != nil {
		t.Fatal("fixture must remain private and owner-controlled:", err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Fatalf("DACL change must preserve cache metadata: %v", err)
	}
	if f, err := platform.OpenPrivateFile(path); err == nil {
		_ = f.Close()
		t.Fatal("fixture must refuse data-read access")
	}
	if s.Status().Enabled {
		t.Fatal("unreadable DACL reused cached on")
	}
}
