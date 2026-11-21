package jobprivacy

import (
	"errors"
	"strings"
	"testing"
)

// The stage_file bytes are the user's data: dropped like a credential from
// every retained copy (citadel-cli#1721).
func TestStagedFileBytesAreRemovedFromRetainedCopies(t *testing.T) {
	const body = "U1lOVEhFVElDLUZJTEUtQllURVM="
	payload := map[string]any{BytesKey: body, "action": "stage_file", "name": "filing.pdf"}
	got := Payload("COBROWSE_SESSION", payload)
	if _, ok := got[BytesKey]; ok || got["name"] != "filing.pdf" || got["action"] != "stage_file" {
		t.Fatalf("retained payload = %#v", got)
	}
	if !HasPasscode(payload) {
		t.Fatal("a bytes field must be treated as a retained-copy secret")
	}
	for _, text := range []string{
		Text("COBROWSE_SESSION", payload, "failed to decode "+body),
		Error("COBROWSE_SESSION", payload, errors.New("bad "+body)).Error(),
		FailureData("COBROWSE_SESSION", payload, errors.New("bad "+body), nil)["error"].(string),
	} {
		if strings.Contains(text, body) {
			t.Fatalf("retained text kept the bytes: %q", text)
		}
	}
	if payload[BytesKey] != body {
		t.Fatal("redaction must not mutate the in-flight payload")
	}
}
