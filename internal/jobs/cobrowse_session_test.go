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

// ---------------------------------------------------------------------------
// #978 session-scoped CDP actions: payload-shape validation and dispatch. None
// of these reach a real browser (they all use an unknown session_id, which the
// manager rejects before any CDP call), so they are hermetic -- exactly the
// same "no such browser session" contract status/stop already use above.
// ---------------------------------------------------------------------------

func wantUnknownSessionErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error for an unknown session_id")
	}
	if !strings.Contains(err.Error(), "no such browser session") {
		t.Errorf("expected a 'no such browser session' error, got %v", err)
	}
}

func TestCobrowseSession_NavigateRequiresSessionID(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action": CobrowseSessionActionNavigate,
		"url":    "https://example.com",
	})
	if err == nil {
		t.Fatal("expected error when navigate has no session_id")
	}
}

func TestCobrowseSession_NavigateRequiresURL(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionNavigate,
		"session_id": "cb-does-not-exist",
	})
	if err == nil {
		t.Fatal("expected error when navigate has no url")
	}
	if strings.Contains(err.Error(), "no such browser session") {
		t.Errorf("missing-url should be caught before the session lookup, got %v", err)
	}
}

func TestCobrowseSession_NavigateUnknownSession(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionNavigate,
		"session_id": "cb-does-not-exist",
		"url":        "https://example.com",
	})
	wantUnknownSessionErr(t, err)
}

func TestCobrowseSession_ScreenshotRequiresSessionID(t *testing.T) {
	_, err := runSession(t, map[string]string{"action": CobrowseSessionActionScreenshot})
	if err == nil {
		t.Fatal("expected error when screenshot has no session_id")
	}
}

func TestCobrowseSession_ScreenshotUnknownSession(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionScreenshot,
		"session_id": "cb-does-not-exist",
	})
	wantUnknownSessionErr(t, err)
}

func TestCobrowseSession_ClickRequiresSessionID(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":   CobrowseSessionActionClick,
		"selector": "#btn",
	})
	if err == nil {
		t.Fatal("expected error when click has no session_id")
	}
}

func TestCobrowseSession_ClickRequiresSelectorOrCoords(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionClick,
		"session_id": "cb-does-not-exist",
	})
	if err == nil {
		t.Fatal("expected error when click has neither selector nor x/y")
	}
	if strings.Contains(err.Error(), "no such browser session") {
		t.Errorf("missing-selector-and-coords should be caught before the session lookup, got %v", err)
	}
}

func TestCobrowseSession_ClickRequiresBothCoords(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionClick,
		"session_id": "cb-does-not-exist",
		"x":          "10",
	})
	if err == nil {
		t.Fatal("expected error when click has 'x' but not 'y'")
	}
}

func TestCobrowseSession_ClickRejectsNonNumericCoords(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionClick,
		"session_id": "cb-does-not-exist",
		"x":          "not-a-number",
		"y":          "10",
	})
	if err == nil {
		t.Fatal("expected error for a non-numeric 'x'")
	}
}

func TestCobrowseSession_ClickUnknownSessionBySelector(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionClick,
		"session_id": "cb-does-not-exist",
		"selector":   "#btn",
	})
	wantUnknownSessionErr(t, err)
}

func TestCobrowseSession_ClickUnknownSessionByCoords(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionClick,
		"session_id": "cb-does-not-exist",
		"x":          "10",
		"y":          "20",
	})
	wantUnknownSessionErr(t, err)
}

func TestCobrowseSession_TypeRequiresSessionID(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action": CobrowseSessionActionType,
		"text":   "hello",
	})
	if err == nil {
		t.Fatal("expected error when type has no session_id")
	}
}

func TestCobrowseSession_TypeRequiresText(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionType,
		"session_id": "cb-does-not-exist",
	})
	if err == nil {
		t.Fatal("expected error when type has no text")
	}
	if strings.Contains(err.Error(), "no such browser session") {
		t.Errorf("missing-text should be caught before the session lookup, got %v", err)
	}
}

func TestCobrowseSession_TypeUnknownSession(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionType,
		"session_id": "cb-does-not-exist",
		"text":       "hello",
	})
	wantUnknownSessionErr(t, err)
}

func TestCobrowseSession_ExtractRequiresSessionID(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":   CobrowseSessionActionExtract,
		"selector": "a",
	})
	if err == nil {
		t.Fatal("expected error when extract has no session_id")
	}
}

func TestCobrowseSession_ExtractRequiresSelector(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionExtract,
		"session_id": "cb-does-not-exist",
	})
	if err == nil {
		t.Fatal("expected error when extract has no selector")
	}
	if strings.Contains(err.Error(), "no such browser session") {
		t.Errorf("missing-selector should be caught before the session lookup, got %v", err)
	}
}

func TestCobrowseSession_ExtractUnknownSession(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionExtract,
		"session_id": "cb-does-not-exist",
		"selector":   "a",
	})
	wantUnknownSessionErr(t, err)
}

func TestCobrowseSession_ExtractParsesAttrsList(t *testing.T) {
	got := parseAttrNames(" href , id ,, class")
	want := []string{"href", "id", "class"}
	if len(got) != len(want) {
		t.Fatalf("parseAttrNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseAttrNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if parseAttrNames("") != nil {
		t.Errorf("parseAttrNames(\"\") should be nil, got %v", parseAttrNames(""))
	}
}

func TestCobrowseSession_HandoffRequiresSessionID(t *testing.T) {
	_, err := runSession(t, map[string]string{"action": CobrowseSessionActionHandoff})
	if err == nil {
		t.Fatal("expected error when handoff has no session_id")
	}
}

func TestCobrowseSession_HandoffUnknownSession(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionHandoff,
		"session_id": "cb-does-not-exist",
	})
	wantUnknownSessionErr(t, err)
}

func TestCobrowseSession_ResumeRequiresSessionID(t *testing.T) {
	_, err := runSession(t, map[string]string{"action": CobrowseSessionActionResume})
	if err == nil {
		t.Fatal("expected error when resume has no session_id")
	}
}

func TestCobrowseSession_ResumeUnknownSession(t *testing.T) {
	_, err := runSession(t, map[string]string{
		"action":     CobrowseSessionActionResume,
		"session_id": "cb-does-not-exist",
	})
	wantUnknownSessionErr(t, err)
}

// TestCobrowseSession_PinNeverLoggedForNewActions is the #978 acceptance
// criterion: none of the six new session-scoped actions read, log, or echo a
// "pin" field, even when a caller mistakenly (or maliciously) supplies one --
// these actions operate on an already-running, already-unlocked session
// located by session_id, so pin handling is simply not in scope for them.
// Drives every new action with a sentinel pin value and asserts it never
// appears in the returned error OR in anything logged via LogFn -- the same
// two surfaces the shipped start/reset/stop actions are already held to.
func TestCobrowseSession_PinNeverLoggedForNewActions(t *testing.T) {
	const sentinelPin = "SUPER-SECRET-PIN-VALUE"

	actions := []map[string]string{
		{"action": CobrowseSessionActionNavigate, "session_id": "cb-nope", "url": "https://example.com"},
		{"action": CobrowseSessionActionScreenshot, "session_id": "cb-nope"},
		{"action": CobrowseSessionActionClick, "session_id": "cb-nope", "selector": "#btn"},
		{"action": CobrowseSessionActionType, "session_id": "cb-nope", "text": "hello"},
		{"action": CobrowseSessionActionExtract, "session_id": "cb-nope", "selector": "a"},
		{"action": CobrowseSessionActionHandoff, "session_id": "cb-nope"},
		{"action": CobrowseSessionActionResume, "session_id": "cb-nope"},
	}

	for _, payload := range actions {
		t.Run(payload["action"], func(t *testing.T) {
			p := map[string]string{"pin": sentinelPin}
			for k, v := range payload {
				p[k] = v
			}

			var logged []string
			h := NewCobrowseSessionHandler()
			ctx := JobContext{LogFn: func(level, msg string) {
				logged = append(logged, msg)
			}}
			_, err := h.Execute(ctx, &nexus.Job{ID: "test", Type: "COBROWSE_SESSION", Payload: p})

			if err != nil && strings.Contains(err.Error(), sentinelPin) {
				t.Errorf("pin leaked into error: %v", err)
			}
			for _, line := range logged {
				if strings.Contains(line, sentinelPin) {
					t.Errorf("pin leaked into log line: %q", line)
				}
			}
		})
	}
}
