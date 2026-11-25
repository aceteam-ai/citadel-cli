package jobprivacy

import "testing"

func TestRetainedDropsLaneMessageOnly(t *testing.T) {
	payload := map[string]any{"message": "text", "offer_id": "o", PasscodeKey: "p"}
	got, changed := Retained(LaneOperation, payload)
	if !changed || got["message"] != nil || got[PasscodeKey] != nil || got["offer_id"] != "o" || payload["message"] != "text" {
		t.Fatalf("retained %v changed=%v input=%v", got, changed, payload)
	}
	if _, changed := Retained(LaneOperation, map[string]any{"operation": "capture"}); changed {
		t.Fatal("capture payload changed")
	}
	if got, changed := Retained("INSTANCE_MESSAGE", map[string]any{"message": "kept"}); changed || got["message"] != "kept" {
		t.Fatal("other job type lost its message")
	}
	if Text(LaneOperation, payload, "saw text here") != "saw [REDACTED] here" {
		t.Fatal("reflected message not redacted")
	}
}
