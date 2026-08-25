package jobs

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

func runSession(t *testing.T, payload map[string]string) ([]byte, error) {
	t.Helper()
	h := NewCobrowseSessionHandler()
	return h.Execute(JobContext{}, &nexus.Job{ID: "test", Type: "COBROWSE_SESSION", Payload: payload})
}

func TestCobrowseSession_MissingAction(t *testing.T) {
	_, err := runSession(t, map[string]string{})
	if err == nil {
		t.Fatal("expected error for missing action")
	}
}

func TestCobrowseSession_UnknownAction(t *testing.T) {
	_, err := runSession(t, map[string]string{"action": "frobnicate"})
	if err == nil {
		t.Fatal("expected error for unknown action")
	}
}

// Status without a session_id lists sessions and is always answerable, even with
// none running: it returns a JSON object with a (possibly empty) "sessions" array.
// This is the queryable-state contract.
func TestCobrowseSession_StatusListIsAlwaysAnswerable(t *testing.T) {
	out, err := runSession(t, map[string]string{"action": CobrowseSessionActionStatus})
	if err != nil {
		t.Fatalf("status list should not error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("status output not JSON: %v", err)
	}
	if _, ok := got["sessions"]; !ok {
		t.Errorf("status list should carry a 'sessions' field, got %v", got)
	}
}

func TestCobrowseSession_StatusUnknownID(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionStatus,
		"session_id": "cb-nope",
	})
	if err == nil {
		t.Fatal("expected error for unknown session id")
	}
}

// A start that names a persistent profile MUST fail closed when no pin is supplied:
// it must never silently fall back to a throwaway (logged-out) session. This is the
// "absent PIN fails closed" acceptance criterion at the job entry point. The guard
// fires before any vault unlock, so no configured vault is needed here. Mutation
// check: a refactor that falls back to StartSession(url) on an absent pin loses the
// "pin" phrase this asserts.
func TestCobrowseSession_StartWithProfileRequiresPIN(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":  CobrowseSessionActionStart,
		"profile": "my-account",
		"url":     "https://example.com",
	})
	if err == nil {
		t.Fatal("expected fail-closed error when a profile is named without a pin")
	}
	if !strings.Contains(err.Error(), "pin") {
		t.Errorf("error should cite the missing pin, got %v", err)
	}
}

func TestCobrowseSession_ResetRequiresProfile(t *testing.T) {
	_, err := runSession(t, map[string]string{"action": CobrowseSessionActionReset})
	if err == nil {
		t.Fatal("expected error when reset has no profile field")
	}
}

func TestCobrowseSession_StopRequiresID(t *testing.T) {
	_, err := runSession(t, map[string]string{"action": CobrowseSessionActionStop})
	if err == nil {
		t.Fatal("expected error when stop has no session_id")
	}
}

// Stopping an unknown session is a no-op (not an error) so a double-stop from the
// backend does not fail the job.
func TestCobrowseSession_StopUnknownIsNoOp(t *testing.T) {
	out, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionStop,
		"session_id": "cb-does-not-exist",
	})
	if err != nil {
		t.Fatalf("stop of unknown session should be a no-op, got %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("stop output not JSON: %v", err)
	}
	if got["stopped"] != "cb-does-not-exist" {
		t.Errorf("stop should echo the id, got %v", got)
	}
}
