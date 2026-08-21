// internal/network/acl.go
// Platform-independent core of the "is this directory attacker-writable"
// check (issue #709). The Win32-specific ACE reading lives in
// acl_windows.go; this file holds the decision logic so it can be unit
// tested without a Windows host.
package network

// aclEntry is a normalized view of one ACCESS_ALLOWED/ACCESS_DENIED ACE from
// a directory's DACL, stripped of Win32 types so evaluateDACL is testable on
// any platform.
type aclEntry struct {
	// sid is the trustee's string SID (e.g. "S-1-5-32-544" for
	// BUILTIN\Administrators), or "S-1-3-0" / "S-1-3-1" for the
	// CREATOR OWNER / CREATOR GROUP placeholders.
	sid string
	// account is a best-effort human-readable name for error messages
	// ("BUILTIN\Users"). Empty if it could not be resolved.
	account string
	// allow is true for an ACCESS_ALLOWED_ACE, false for ACCESS_DENIED.
	// evaluateDACL only acts on allow entries -- see its doc comment for why
	// deny entries are deliberately ignored rather than modeled precisely.
	allow bool
	// mask is the ACE's raw Windows ACCESS_MASK.
	mask uint32
}

// Access mask bits relevant to "can this trustee change what's on disk
// here". WRITE_DAC and WRITE_OWNER are included even though they don't
// directly write file bytes: either lets a non-admin holder grant themselves
// write access afterward, which is the same hole one step removed.
const (
	maskFileWriteData       = 0x0002 // FILE_WRITE_DATA / FILE_ADD_FILE
	maskFileAppendData      = 0x0004 // FILE_APPEND_DATA / FILE_ADD_SUBDIRECTORY
	maskFileWriteEA         = 0x0010 // FILE_WRITE_EA
	maskFileDeleteChild     = 0x0040 // FILE_DELETE_CHILD (directories)
	maskFileWriteAttributes = 0x0100 // FILE_WRITE_ATTRIBUTES
	maskDelete              = 0x00010000
	maskWriteDAC            = 0x00040000
	maskWriteOwner          = 0x00080000
	maskGenericAll          = 0x10000000
	maskGenericWrite        = 0x40000000
)

const writeCapableMask = maskFileWriteData | maskFileAppendData | maskFileWriteEA |
	maskFileDeleteChild | maskFileWriteAttributes | maskDelete | maskWriteDAC |
	maskWriteOwner | maskGenericAll | maskGenericWrite

// Well-known SIDs that are allowed to hold write/delete/take-ownership
// rights on a machine-wide install directory without tripping the check.
// String forms per Microsoft's well-known SID list; these are locale- and
// language-independent.
const (
	sidAdministrators   = "S-1-5-32-544" // BUILTIN\Administrators
	sidLocalSystem      = "S-1-5-18"     // NT AUTHORITY\SYSTEM
	sidTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	sidCreatorOwner     = "S-1-3-0" // placeholder: resolves to the object's actual owner
	sidCreatorGroup     = "S-1-3-1" // placeholder: resolves to the object's actual group
)

func isAdminLikeSID(sid string) bool {
	switch sid {
	case sidAdministrators, sidLocalSystem, sidTrustedInstaller:
		return true
	default:
		return false
	}
}

// evaluateDACL returns the entries that grant a non-admin principal
// write-capable access, i.e. the reasons machine-wide mode must refuse to
// load a driver from this directory.
//
// ownerSID resolves the CREATOR OWNER / CREATOR GROUP placeholder SIDs,
// which name "whoever owns/groups this object" rather than a concrete
// principal -- a fresh, uninherited ACE commonly uses them.
//
// Deliberately conservative, matching the fail-closed rule the rest of
// machine-wide mode already follows (ErrNeedsElevation never falls back to
// userspace): this only looks at ACCESS_ALLOWED entries and ignores
// ACCESS_DENIED ones entirely, rather than simulating Windows' actual
// deny-before-allow evaluation order. A DENY ace that would, in a full
// access-check simulation, cancel out a matching ALLOW is not enough to
// clear this check -- an administrator could remove that DENY ace at any
// time without our knowledge, and the failure mode of trusting it (loading
// an attacker-plantable DLL into an elevated process) is far worse than the
// failure mode of a false positive (an operator sees "reinstall for
// machine-wide mode" on a directory that was actually fine).
func evaluateDACL(entries []aclEntry, ownerSID string) []aclEntry {
	var violations []aclEntry
	for _, e := range entries {
		if !e.allow {
			continue
		}
		if e.mask&writeCapableMask == 0 {
			continue
		}
		if e.sid == sidCreatorOwner || e.sid == sidCreatorGroup {
			if ownerSID == "" {
				// Owner unknown: fail closed rather than assume it's admin.
				violations = append(violations, e)
				continue
			}
			// Report the RESOLVED principal, not the CREATOR OWNER/GROUP
			// placeholder, so a caller's error message names who actually
			// has the access rather than an opaque well-known SID.
			e.sid = ownerSID
		}
		if isAdminLikeSID(e.sid) {
			continue
		}
		violations = append(violations, e)
	}
	return violations
}
