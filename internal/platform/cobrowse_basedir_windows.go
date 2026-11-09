//go:build windows

// internal/platform/cobrowse_basedir_windows.go
//
// Windows counterpart of cobrowse_basedir_unix.go (aceteam#13503 S1). The co-browse
// session base dir holds every session's browser profile, so it must be private
// to the current user. On Windows that is an ownership plus DACL property, not a
// mode bit: the dir is created with an explicit, protected (non-inherited) DACL
// whose only grant is full control for the current user, inherited by every
// session dir and file beneath it. An existing dir must still have exactly that
// shape; anything else is refused, never repaired in place.
package platform

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// currentUserSID returns the SID of the user the current process runs as.
func currentUserSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read current user: %w", err)
	}
	return u.User.Sid.Copy()
}

// ownerOnlyACL builds a DACL granting full control to sid only, inherited by
// child dirs and files.
func ownerOnlyACL(sid *windows.SID) (*windows.ACL, error) {
	return windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
}

// secureNewCobrowseBaseDir sets the current user as owner and replaces the
// inherited ACL with a protected owner-only DACL.
func secureNewCobrowseBaseDir(dir string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	acl, err := ownerOnlyACL(sid)
	if err != nil {
		return fmt.Errorf("build owner-only ACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		sid, nil, acl, nil); err != nil {
		return fmt.Errorf("set owner-only permissions on %s: %w", dir, err)
	}
	return nil
}

// validateCobrowseBaseDirPerms confirms dir is owned by the current user, does
// not inherit permissions from its parent, and grants access to nobody else.
// Fails closed on any read error or unrecognized ACE.
func validateCobrowseBaseDirPerms(dir string, _ os.FileInfo) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("cobrowse base dir %s: read permissions: %w", dir, err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("cobrowse base dir %s: could not determine owner", dir)
	}
	if !owner.Equals(user) {
		return fmt.Errorf("cobrowse base dir %s is owned by %s, not the current user %s; refusing to use it", dir, owner, user)
	}
	control, _, err := sd.Control()
	if err != nil {
		return fmt.Errorf("cobrowse base dir %s: read security control: %w", dir, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("cobrowse base dir %s inherits permissions from its parent; refusing to use it", dir)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("cobrowse base dir %s: read DACL: %w", dir, err)
	}
	if dacl == nil {
		return fmt.Errorf("cobrowse base dir %s has no DACL (everyone has access); refusing to use it", dir)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("cobrowse base dir %s: read ACE %d: %w", dir, i, err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.Equals(user) {
				return fmt.Errorf("cobrowse base dir %s grants access to %s; only the current user may have access", dir, sid)
			}
		default:
			return fmt.Errorf("cobrowse base dir %s has an unsupported ACE type %d; refusing to use it", dir, ace.Header.AceType)
		}
	}
	return nil
}
