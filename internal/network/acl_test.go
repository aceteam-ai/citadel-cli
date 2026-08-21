// internal/network/acl_test.go
package network

import "testing"

func TestEvaluateDACL(t *testing.T) {
	const (
		sidUsers             = "S-1-5-32-545" // BUILTIN\Users
		sidEveryone          = "S-1-1-0"
		sidAuthenticatedUser = "S-1-5-11"
		sidSomeUser          = "S-1-5-21-1111111111-2222222222-3333333333-1001"
	)

	tests := []struct {
		name              string
		entries           []aclEntry
		ownerSID          string
		wantViolationSIDs []string
	}{
		{
			name: "admin and system only -- clean",
			entries: []aclEntry{
				{sid: sidAdministrators, allow: true, mask: maskGenericAll},
				{sid: sidLocalSystem, allow: true, mask: maskGenericAll},
			},
			wantViolationSIDs: nil,
		},
		{
			name: "trusted installer full control -- clean",
			entries: []aclEntry{
				{sid: sidTrustedInstaller, allow: true, mask: maskGenericAll},
				{sid: sidAdministrators, allow: true, mask: 0x1200A9}, // read & execute only
			},
			wantViolationSIDs: nil,
		},
		{
			name: "admin has read-only, no write bits -- clean",
			entries: []aclEntry{
				{sid: sidAdministrators, allow: true, mask: 0x1200A9},
			},
			wantViolationSIDs: nil,
		},
		{
			name: "Users group granted write -- violation (the %LOCALAPPDATA% case)",
			entries: []aclEntry{
				{sid: sidAdministrators, allow: true, mask: maskGenericAll},
				{sid: sidUsers, allow: true, mask: maskFileWriteData},
			},
			wantViolationSIDs: []string{sidUsers},
		},
		{
			name: "Everyone granted delete -- violation",
			entries: []aclEntry{
				{sid: sidEveryone, allow: true, mask: maskDelete},
			},
			wantViolationSIDs: []string{sidEveryone},
		},
		{
			name: "specific non-admin user granted WRITE_DAC -- violation (can self-grant later)",
			entries: []aclEntry{
				{sid: sidAdministrators, allow: true, mask: maskGenericAll},
				{sid: sidSomeUser, allow: true, mask: maskWriteDAC},
			},
			wantViolationSIDs: []string{sidSomeUser},
		},
		{
			name: "authenticated users granted append -- violation",
			entries: []aclEntry{
				{sid: sidAuthenticatedUser, allow: true, mask: maskFileAppendData},
			},
			wantViolationSIDs: []string{sidAuthenticatedUser},
		},
		{
			name: "deny entry for a non-admin is NOT enough to clear an allow elsewhere",
			entries: []aclEntry{
				{sid: sidUsers, allow: false, mask: maskGenericAll},
				{sid: sidUsers, allow: true, mask: maskFileWriteData},
			},
			wantViolationSIDs: []string{sidUsers},
		},
		{
			name: "deny-only entries grant nothing -- clean",
			entries: []aclEntry{
				{sid: sidUsers, allow: false, mask: maskGenericAll},
			},
			wantViolationSIDs: nil,
		},
		{
			name: "CREATOR OWNER resolved to an admin owner -- clean",
			entries: []aclEntry{
				{sid: sidCreatorOwner, allow: true, mask: maskGenericAll},
			},
			ownerSID:          sidAdministrators,
			wantViolationSIDs: nil,
		},
		{
			name: "CREATOR OWNER resolved to a non-admin owner -- violation",
			entries: []aclEntry{
				{sid: sidCreatorOwner, allow: true, mask: maskFileWriteData},
			},
			ownerSID:          sidSomeUser,
			wantViolationSIDs: []string{sidSomeUser},
		},
		{
			name: "CREATOR OWNER with unknown owner -- fail closed",
			entries: []aclEntry{
				{sid: sidCreatorOwner, allow: true, mask: maskFileWriteData},
			},
			ownerSID:          "",
			wantViolationSIDs: []string{sidCreatorOwner},
		},
		{
			name:              "no entries at all -- clean",
			entries:           nil,
			wantViolationSIDs: nil,
		},
		{
			name: "read-only mask bits on a non-admin -- clean (not write-capable)",
			entries: []aclEntry{
				{sid: sidUsers, allow: true, mask: 0x1200A9}, // FILE_GENERIC_READ | FILE_GENERIC_EXECUTE
			},
			wantViolationSIDs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateDACL(tt.entries, tt.ownerSID)
			gotSIDs := make([]string, 0, len(got))
			for _, e := range got {
				gotSIDs = append(gotSIDs, e.sid)
			}
			if len(gotSIDs) != len(tt.wantViolationSIDs) {
				t.Fatalf("evaluateDACL() = %v, want violations for %v", gotSIDs, tt.wantViolationSIDs)
			}
			for i, want := range tt.wantViolationSIDs {
				if gotSIDs[i] != want {
					t.Errorf("violation[%d] sid = %q, want %q", i, gotSIDs[i], want)
				}
			}
		})
	}
}

func TestIsAdminLikeSID(t *testing.T) {
	admin := []string{sidAdministrators, sidLocalSystem, sidTrustedInstaller}
	for _, sid := range admin {
		if !isAdminLikeSID(sid) {
			t.Errorf("isAdminLikeSID(%q) = false, want true", sid)
		}
	}

	nonAdmin := []string{"S-1-5-32-545", "S-1-1-0", "S-1-5-11", "S-1-3-0", "S-1-3-1", ""}
	for _, sid := range nonAdmin {
		if isAdminLikeSID(sid) {
			t.Errorf("isAdminLikeSID(%q) = true, want false", sid)
		}
	}
}
