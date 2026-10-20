package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDesktopGateReason(t *testing.T) {
	const pin = "4821"
	verify := func(p string) bool { return p == pin }
	state := func(set bool, v func(string) bool) func() (PasscodeState, error) {
		return func() (PasscodeState, error) { return PasscodeState{Set: set, Check: BoolCheck(v)}, nil }
	}
	for _, tc := range []struct {
		name      string
		gate      DesktopGate
		presented string
		want      string
	}{
		{"no passcode set allows", DesktopGate{Load: state(false, verify)}, "", ""},
		{"no passcode set ignores a presented value", DesktopGate{Load: state(false, verify)}, "anything", ""},
		{"passcode set, none presented", DesktopGate{Load: state(true, verify)}, "", ReasonPasscodeInvalid},
		{"passcode set, wrong one presented", DesktopGate{Load: state(true, verify)}, "0000", ReasonPasscodeInvalid},
		{"passcode set, correct one presented", DesktopGate{Load: state(true, verify)}, pin, ""},
		{"nil Load refuses", DesktopGate{}, pin, ReasonPasscodeUnavailable},
		{"nil Check with a passcode set refuses", DesktopGate{Load: state(true, nil)}, pin, ReasonPasscodeInvalid},
		{"a read error refuses even with the correct passcode", DesktopGate{Load: func() (PasscodeState, error) {
			return PasscodeState{}, errors.New("permission denied")
		}}, pin, ReasonPasscodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DesktopGateReason(tc.gate, tc.presented); got != tc.want {
				t.Fatalf("DesktopGateReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDesktopGateReadsStateOncePerDecision(t *testing.T) {
	loads := 0
	g := DesktopGate{Load: func() (PasscodeState, error) {
		loads++
		return PasscodeState{Set: true, Check: BoolCheck(func(p string) bool { return p == "1" })}, nil
	}}
	DesktopGateReason(g, "1")
	if loads != 1 {
		t.Fatalf("Load called %d times, want 1 per decision", loads)
	}
}

func TestDesktopRefusalIsTheSharedRefusalShape(t *testing.T) {
	for _, reason := range []string{ReasonPasscodeInvalid, ReasonPasscodeUnavailable} {
		var parsed struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(DesktopRefusal(reason).Error()), &parsed); err != nil {
			t.Fatalf("refusal is not JSON: %v", err)
		}
		if parsed.Reason != reason || parsed.Message == "" {
			t.Fatalf("refusal = %+v, want reason %s and a message", parsed, reason)
		}
	}
	if !strings.Contains(DesktopRefusal(ReasonPasscodeInvalid).Message, "desktop") {
		t.Fatal("the invalid-passcode message should name the desktop surface")
	}
}
