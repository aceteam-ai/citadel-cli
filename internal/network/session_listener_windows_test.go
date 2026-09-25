//go:build windows

package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsRawProbeMissingPipeReturnsPromptly(t *testing.T) {
	path := fmt.Sprintf(`\\.\pipe\citadel-session-absent-%d-%d`, os.Getpid(), time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	if conn, err := dialLocalControlRaw(ctx, path); !errors.Is(err, os.ErrNotExist) {
		if conn != nil {
			conn.Close()
		}
		t.Fatalf("absent pipe probe = %v; want immediate not-exist, not retry/timeout", err)
	}
}

func TestWindowsSessionPipeACLExcludesOrdinaryUsers(t *testing.T) {
	path := LocalAPISocketPath("")
	ln, err := listenSessionControlEndpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("session pipe has no explicit DACL: %v", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	want[user.User.Sid.String()]++
	want["S-1-5-18"]++     // Local System; may be the owner in a service.
	want["S-1-5-32-544"]++ // Administrators
	if dacl.AceCount != 3 {
		t.Fatalf("session pipe has %d ACEs; want only owner, SYSTEM and Administrators", dacl.AceCount)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("unexpected ACE type %d", ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if ace.Mask != windows.GENERIC_ALL {
			t.Fatalf("session pipe ACE for %s has mask %#x; want GENERIC_ALL", sid, ace.Mask)
		}
		want[sid]--
		if want[sid] < 0 {
			t.Fatalf("unexpected or duplicate session pipe ACE for %s", sid)
		}
	}
	for sid, remaining := range want {
		if remaining != 0 {
			t.Fatalf("session pipe is missing ACE for %s", sid)
		}
	}
}

func TestWindowsMachineWidePipeRetainsBuiltinUsersAttach(t *testing.T) {
	path := LocalAPISocketPath("")
	ln, err := listenLocalAPI(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("machine-wide pipe has no DACL: %v", err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if sid == "S-1-5-32-545" { // Builtin Users, the pre-S2 TUN attach policy.
			if ace.Mask&(windows.GENERIC_READ|windows.GENERIC_WRITE) != windows.GENERIC_READ|windows.GENERIC_WRITE {
				t.Fatalf("machine-wide Builtin Users ACE lost read/write access: %#x", ace.Mask)
			}
			return
		}
	}
	t.Fatal("machine-wide TUN pipe lost its Builtin Users attach ACE")
}
