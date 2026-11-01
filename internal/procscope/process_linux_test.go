//go:build linux

package procscope

import (
	"strings"
	"testing"
)

func TestProcessIdentityParsingIsNotPIDFileAuthority(t *testing.T) {
	fields := append([]string{"S"}, strings.Fields(strings.Repeat("0 ", 18))...)
	fields = append(fields, "12345")
	stat := "42 (name with ) and spaces) " + strings.Join(fields, " ")
	if start, err := parseProcessStat([]byte(stat), 42); err != nil || start != 12345 {
		t.Fatalf("birth parsing: start=%d err=%v", start, err)
	}
	for _, raw := range []string{"42 malformed", "42 (x) S", strings.Replace(stat, "12345", "0", 1)} {
		if _, err := parseProcessStat([]byte(raw), 42); err == nil {
			t.Fatal("malformed birth accepted")
		}
	}
	if _, err := parseProcessStat([]byte(stat), 43); err == nil {
		t.Fatal("different PID accepted")
	}
	if uid, err := parseProcessUID([]byte("Name: x\nUid:\t1000\t1000\t1000\t1000\n")); err != nil || uid != 1000 {
		t.Fatal("ordinary same-UID identity refused")
	}
	for _, raw := range []string{"Uid: 0 0 0 0", "Uid: 1000 0 1000 1000", "Uid: 1000", "Gid: 1000 1000 1000 1000"} {
		if _, err := parseProcessUID([]byte(raw)); err == nil {
			t.Fatal("root, credential transition or missing UID accepted")
		}
	}
}

func TestUnifiedCgroupProofRefusesV1AndAmbiguity(t *testing.T) {
	want := "/user.slice/user-1000.slice/user@1000.service/citadel-sess-a.scope"
	if group, err := parseUnifiedCgroup([]byte("0::" + want + "\n")); err != nil || group != want {
		t.Fatal("unified membership refused")
	}
	for _, raw := range []string{"", "0::/", "1:name=systemd:/x", "0::/x\n1:cpu:/x", "0::/x\n0::/x", "0::/x/../y", "0::relative"} {
		if _, err := parseUnifiedCgroup([]byte(raw)); err == nil {
			t.Fatalf("unsupported or ambiguous membership accepted: %q", raw)
		}
	}
}
