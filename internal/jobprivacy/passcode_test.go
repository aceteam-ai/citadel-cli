package jobprivacy

import (
	"errors"
	"strings"
	"testing"
)

func TestPasscodeIsRemovedFromRetainedCopies(t *testing.T) {
	const pin = "synthetic-node-passcode-4821"
	payload := map[string]any{PasscodeKey: pin, "command": "true"}
	for _, kind := range []string{"SHELL_COMMAND", "VNC_ACTIONS", "WORKFLOW_RUN", "SOMETHING_ELSE"} {
		got := Payload(kind, payload)
		if _, ok := got[PasscodeKey]; ok || got["command"] != "true" {
			t.Fatalf("%s: retained payload = %#v", kind, got)
		}
		if strings.Contains(Text(kind, payload, "refused: "+pin), pin) {
			t.Fatalf("%s: text kept the passcode", kind)
		}
		if strings.Contains(Error(kind, payload, errors.New("bad "+pin)).Error(), pin) {
			t.Fatalf("%s: error kept the passcode", kind)
		}
		if strings.Contains(FailureData(kind, payload, errors.New("bad "+pin), nil)["error"].(string), pin) {
			t.Fatalf("%s: failure data kept the passcode", kind)
		}
	}
	if payload[PasscodeKey] != pin {
		t.Fatal("redaction must not mutate the in-flight payload")
	}
	if Payload(FileFetch, payload) != nil {
		t.Fatal("FILE_FETCH payloads stay fully omitted")
	}
	plain := map[string]any{"command": "true"}
	if got := Payload("SHELL_COMMAND", plain); len(got) != 1 || got["command"] != "true" {
		t.Fatalf("a payload without a passcode must be unchanged: %#v", got)
	}
}

// TestCobrowseProfilePINIsRemovedFromRetainedCopies: the COBROWSE_SESSION
// start "pin" unlocks a persistent profile (an at-rest passphrase since #820)
// and is dropped like the passcode.
func TestCobrowseProfilePINIsRemovedFromRetainedCopies(t *testing.T) {
	const secret = "synthetic profile passphrase words"
	payload := map[string]any{PINKey: secret, "action": "start", "profile": "default"}
	got := Payload("COBROWSE_SESSION", payload)
	if _, ok := got[PINKey]; ok || got["profile"] != "default" || got["action"] != "start" {
		t.Fatalf("retained payload = %#v", got)
	}
	if !HasPasscode(payload) {
		t.Fatal("a pin field must count as a credential")
	}
	for _, text := range []string{
		Text("COBROWSE_SESSION", payload, "unlock failed for "+secret),
		Error("COBROWSE_SESSION", payload, errors.New("bad "+secret)).Error(),
		FailureData("COBROWSE_SESSION", payload, errors.New("bad "+secret), nil)["error"].(string),
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("retained text kept the pin: %q", text)
		}
	}
	if payload[PINKey] != secret {
		t.Fatal("redaction must not mutate the in-flight payload")
	}
}
