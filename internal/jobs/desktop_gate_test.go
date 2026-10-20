package jobs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDesktopGateReason(t *testing.T) {
	const pin = "4821"
	verify := func(p string) bool { return p == pin }
	set := func() bool { return true }
	unset := func() bool { return false }
	for _, tc := range []struct {
		name      string
		gate      DesktopGate
		presented string
		want      string
	}{
		{"no passcode set allows", DesktopGate{HasPasscode: unset, VerifyPasscode: verify}, "", ""},
		{"no passcode set ignores a presented value", DesktopGate{HasPasscode: unset, VerifyPasscode: verify}, "anything", ""},
		{"passcode set, none presented", DesktopGate{HasPasscode: set, VerifyPasscode: verify}, "", ReasonPasscodeInvalid},
		{"passcode set, wrong one presented", DesktopGate{HasPasscode: set, VerifyPasscode: verify}, "0000", ReasonPasscodeInvalid},
		{"passcode set, correct one presented", DesktopGate{HasPasscode: set, VerifyPasscode: verify}, pin, ""},
		{"nil HasPasscode fails closed", DesktopGate{VerifyPasscode: verify}, pin, ReasonPasscodeInvalid},
		{"nil VerifyPasscode with a passcode set fails closed", DesktopGate{HasPasscode: set}, pin, ReasonPasscodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DesktopGateReason(tc.gate, tc.presented); got != tc.want {
				t.Fatalf("DesktopGateReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDesktopRefusalIsTheSharedRefusalShape(t *testing.T) {
	var parsed struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(DesktopRefusal(ReasonPasscodeInvalid).Error()), &parsed); err != nil {
		t.Fatalf("refusal is not JSON: %v", err)
	}
	if parsed.Reason != ReasonPasscodeInvalid || !strings.Contains(parsed.Message, "desktop") {
		t.Fatalf("refusal = %+v, want reason passcode_invalid and a desktop message", parsed)
	}
}
