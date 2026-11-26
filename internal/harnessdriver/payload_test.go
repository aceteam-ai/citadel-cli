package harnessdriver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTypedPayloadsUseTheEmbeddedContract(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		payload any
	}{
		{"assistant.text", Text{Text: "synthetic"}},
		{"assistant.delta", Text{Text: "delta"}},
		{"tool.activity", Tool{ID: "tool-1", State: "started"}},
		{"approval.requested", Approval{ID: "approval-1", Kind: "file_change", Choices: []string{"decline"}}},
		{"approval.resolved", Resolution{ID: "approval-1", Decision: "decline"}},
		{"usage.reported", Usage{}},
		{"quota.reported", Quota{Window: "unknown"}},
		{"models.reported", Models{IDs: []string{}}},
		{"turn.completed", Completed{Outcome: "unknown"}},
		{"session.error", Failure{Code: "unsupported"}},
	} {
		e := sampleEvent(1)
		e.Type = tc.kind
		var err error
		e.Data, err = json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Validate(); err != nil {
			t.Fatalf("typed %s does not match schema: %v", tc.kind, err)
		}
	}
	for _, tc := range []struct {
		kind    string
		payload any
	}{
		{"session.start", Start{Workspace: "/synthetic/workspace", Model: "synthetic-model"}},
		{"turn.submit", Submit{Text: "synthetic"}},
		{"approval.respond", Respond{ApprovalID: "approval-1", Decision: "decline"}},
		{"turn.interrupt", Empty{}}, {"session.stop", Empty{}}, {"session.probe", Empty{}},
	} {
		raw, err := json.Marshal(tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		c := Command{Protocol: Version, Type: tc.kind, SessionName: "synthetic", Incarnation: "generation-1", RequestID: "request-1", Data: raw}
		if err = c.Validate(); err != nil {
			t.Fatalf("typed %s: %v", tc.kind, err)
		}
		c.RequestID = ""
		if err = c.Validate(); !errors.Is(err, ErrProtocol) {
			t.Fatal("unbound command accepted")
		}
	}
}

func TestDecodeFailureReturnsNoPartialEnvelope(t *testing.T) {
	e := sampleEvent(1)
	raw, _ := json.Marshal(e)
	for _, bad := range [][]byte{
		append(append([]byte{}, raw...), raw...),
		[]byte(strings.Replace(string(raw), `"seq":1`, `"seq":1.0`, 1)),
		[]byte(strings.Replace(string(raw), `"ts":"2026-01-01T00:00:00Z"`, `"ts":"not-time"`, 1)),
		append([]byte{0xff}, raw...),
	} {
		got, err := DecodeEvent(bad)
		if !errors.Is(err, ErrProtocol) || got.Protocol != "" || got.Data != nil {
			t.Fatalf("partial envelope returned: %+v %v", got, err)
		}
	}
	e.Data = make([]byte, MaxEnvelopeBytes+1)
	if err := e.Validate(); !errors.Is(err, ErrProtocol) {
		t.Fatal("oversized typed event accepted")
	}
	if SchemaJSON() == "" {
		t.Fatal("missing embedded schema")
	}
	if _, err := NewEventQueue(0); !errors.Is(err, ErrInvalidLimit) {
		t.Fatal(err)
	}
	if _, err := NewEventQueue(65); !errors.Is(err, ErrInvalidLimit) {
		t.Fatal(err)
	}
}
