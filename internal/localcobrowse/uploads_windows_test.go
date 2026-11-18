//go:build windows

package localcobrowse

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
	"golang.org/x/sys/windows"
)

func uploadMetadataSnapshot(t *testing.T, path string) os.FileInfo {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal("metadata handle:", err)
	}
	f := os.NewFile(uintptr(h), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal("metadata stat:", err)
	}
	return st
}

func TestUploadsCachedFileBecomingUnreadableDACL(t *testing.T) {
	dir, s := uploadFixture(t)
	if !s.Status().Enabled {
		t.Fatal("fixture")
	}
	path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
	before := uploadMetadataSnapshot(t, path)
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
	// Keep only owner metadata/ACL access, not file data access. This lets
	// Lstat and SameFile still inspect unchanged metadata after revocation.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	const metadataAccess = windows.READ_CONTROL | windows.WRITE_DAC | windows.FILE_READ_ATTRIBUTES
	denied, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;;0x%x;;;%s)", metadataAccess, user.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	metadataACL, _, err := denied.DACL()
	if err != nil || metadataACL == nil || metadataACL.AceCount != 1 {
		t.Fatalf("metadata-only DACL fixture: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, metadataACL, nil); err != nil {
		t.Fatal(err)
	}
	if err := platform.ValidatePrivateFile(path); err != nil {
		t.Fatal("fixture must remain private and owner-controlled:", err)
	}
	after := uploadMetadataSnapshot(t, path)
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		t.Fatal("DACL change must preserve cache metadata")
	}
	if f, err := platform.OpenPrivateFile(path); err == nil {
		_ = f.Close()
		t.Fatal("fixture must refuse data-read access")
	}
	if s.Status().Enabled {
		t.Fatal("unreadable DACL reused cached on")
	}
}

func TestUploadsWindowsGateAllowsSharedReadHandle(t *testing.T) {
	dir, s := uploadFixture(t)
	path := filepath.Join(dir, UploadSettingsDir, UploadSettingsFile)
	f, err := platform.OpenPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// A lazy Lstat identity lookup uses an exclusive Windows open and fails
	// deterministically while this read handle is live. The gate must not.
	if !s.Status().Enabled || !s.Status().Enabled {
		t.Fatal("gate required an exclusive identity lookup")
	}
	if err := WriteUploadSetting(dir, "off"); err != nil {
		t.Fatal("shared gate handle blocked atomic replacement:", err)
	}
	if s.Status().Enabled {
		t.Fatal("replacement opt-out lost with an old read handle alive")
	}
}
