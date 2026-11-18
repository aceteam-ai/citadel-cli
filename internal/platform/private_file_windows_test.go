//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateSettingsFileRequiresProtectedOwnerOnlyDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"uploads":"on"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SecurePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	f, err := OpenPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// A Chmod-only file would leave inheritance enabled. Even an otherwise
	// owner-only ACL must be protected before it is accepted as settings.
	original, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	originalACL, _, err := original.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, originalACL, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateFile(path); err == nil {
		t.Fatal("unprotected DACL accepted")
	}
	if f, err := OpenPrivateFile(path); err == nil {
		_ = f.Close()
		t.Fatal("unprotected descriptor DACL accepted")
	}
	if err := SecurePrivateFile(path); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateFile(path); err == nil {
		t.Fatal("non-owner allow ACE accepted")
	}
	if f, err := OpenPrivateFile(path); err == nil {
		_ = f.Close()
		t.Fatal("non-owner descriptor DACL accepted")
	}
}

func TestWindowsSettingsParentRejectsOtherWriter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "container")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSettingsParent(dir); err != nil {
		t.Fatal("inherited legacy container refused:", err)
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, rights := range []string{"FR", "0x00000040", "FW", "WD", "WO"} {
		sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.String() + ")(A;;" + rights + ";;;WD)")
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			t.Fatal(err)
		}
		err = ValidateSettingsParent(dir)
		if (err == nil) != (rights == "FR") {
			t.Fatalf("rights %s accepted=%v", rights, err == nil)
		}
	}
}
