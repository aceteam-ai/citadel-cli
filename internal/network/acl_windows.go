//go:build windows

// internal/network/acl_windows.go
// Win32 DACL reading for the machine-wide install-directory check (issue
// #709). The decision logic (evaluateDACL) is platform-independent and lives
// in acl.go so it can be unit tested without a Windows host; this file's job
// is only to turn a real directory into the []aclEntry shape that logic
// consumes.
package network

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// directoryACLEntries reads the DACL of path and returns it as normalized
// aclEntry values, plus the resolved owner SID (used to resolve the
// CREATOR OWNER / CREATOR GROUP placeholder SIDs in evaluateDACL, and to
// independently assert the owner itself is admin-like -- see
// ownerNotAdminLike) and a best-effort human-readable owner account name for
// error messages.
func directoryACLEntries(path string) (entries []aclEntry, ownerSID string, ownerAccount string, err error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, "", "", fmt.Errorf("read security descriptor: %w", err)
	}
	if sd == nil {
		return nil, "", "", fmt.Errorf("no security descriptor for %s", path)
	}

	if owner, _, oerr := sd.Owner(); oerr == nil && owner != nil {
		ownerSID = owner.String()
		if account, domain, _, lerr := owner.LookupAccount(""); lerr == nil {
			if domain != "" {
				ownerAccount = domain + `\` + account
			} else {
				ownerAccount = account
			}
		}
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, ownerSID, ownerAccount, fmt.Errorf("read DACL: %w", err)
	}
	if dacl == nil {
		// A null DACL means "no protection: everyone has full access" -- the
		// most permissive state a securable object can be in. Model it as an
		// explicit violation rather than reading "we found zero ACEs" as
		// "nothing is wrong", which would be exactly backwards.
		return []aclEntry{{
			sid:     "S-1-1-0",
			account: "Everyone (no DACL present on this object)",
			allow:   true,
			mask:    maskGenericAll,
		}}, ownerSID, ownerAccount, nil
	}

	entries = make([]aclEntry, 0, dacl.AceCount)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, ownerSID, ownerAccount, fmt.Errorf("read ACE %d: %w", i, err)
		}

		// ACCESS_ALLOWED_ACE and ACCESS_DENIED_ACE share this exact layout
		// (header, mask, then the SID); object/callback/audit ACE types do
		// not, so skip anything else rather than misinterpret it.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}

		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		entry := aclEntry{
			sid:   sid.String(),
			mask:  uint32(ace.Mask),
			allow: ace.Header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE,
		}
		if account, domain, _, lerr := sid.LookupAccount(""); lerr == nil {
			if domain != "" {
				entry.account = domain + `\` + account
			} else {
				entry.account = account
			}
		}
		entries = append(entries, entry)
	}
	return entries, ownerSID, ownerAccount, nil
}

// checkDirectoryNotWritableByNonAdmin refuses to proceed if any non-admin
// principal can write, create, delete, or take ownership within dir --
// which for a directory holding a loadable DLL beside an elevated citadel
// binary is a privilege-escalation path, not just a tampering risk (see
// docs/machine-wide-tun.md, "The security problem this creates"). Fails
// closed: any error reading the ACL is treated as a refusal, never as a
// pass.
//
// Two independent checks must both pass (issue #789): the DACL-based walk
// (evaluateDACL) AND an explicit assertion that the directory's OWNER is
// itself admin-like (ownerNotAdminLike). The owner check exists because an
// object's owner holds implicit READ_CONTROL/WRITE_DAC no matter what the
// DACL says, so a non-admin owner is a live privilege-escalation path even
// when every current ACE looks clean -- that owner could re-DACL the
// directory to grant itself write access at any time.
func checkDirectoryNotWritableByNonAdmin(dir string) error {
	entries, ownerSID, ownerAccount, err := directoryACLEntries(dir)
	if err != nil {
		return fmt.Errorf(
			"machine-wide mode refused: could not verify %s is admin-only (%v).\n"+
				"   Reinstall citadel to %%ProgramFiles%%\\Citadel for machine-wide mode.",
			dir, err)
	}

	if ownerNotAdminLike(ownerSID) {
		who := ownerAccount
		if who == "" {
			who = ownerSID
		}
		if who == "" {
			who = "an unresolved principal"
		}
		return fmt.Errorf(
			"machine-wide mode refused: %s is owned by %s, not an administrator.\n"+
				"   An object's owner can rewrite its permissions at any time regardless of the current ACL,\n"+
				"   so a non-admin owner is a privilege-escalation path even if today's ACL looks fine. This\n"+
				"   can happen on an install that predates this check even though its ACL is otherwise fine\n"+
				"   (icacls locks down the ACL but does not reassign ownership on its own).\n"+
				"   Fix it from an elevated prompt:  icacls \"%s\" /setowner \"*S-1-5-32-544\"\n"+
				"   'citadel login' keeps working from the current install either way.",
			dir, who, dir)
	}

	violations := evaluateDACL(entries, ownerSID)
	if len(violations) == 0 {
		return nil
	}

	who := violations[0].account
	if who == "" {
		who = violations[0].sid
	}
	return fmt.Errorf(
		"machine-wide mode refused: %s can be modified by %s, a non-administrator.\n"+
			"   'citadel up' runs elevated, so a directory a non-admin can write to is a privilege-escalation\n"+
			"   path (they could plant a driver it would load). Reinstall citadel to %%ProgramFiles%%\\Citadel\n"+
			"   for machine-wide mode -- 'citadel login' keeps working from the current install either way.",
		dir, who)
}
