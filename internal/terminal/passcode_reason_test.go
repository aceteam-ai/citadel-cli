// internal/terminal/passcode_reason_test.go
//
// Covers citadel#753: the passcode-gate reject response must distinguish
// ReasonPasscodeNotSet (no node passcode configured at all) from
// ReasonPasscodeInvalid (a passcode is configured but the wrong one, or none,
// was presented), so a client can show actionable text instead of a generic
// "node passcode required". See internal/jobs/shell_command.go for the
// mirrored SHELL_COMMAND behavior this pattern is modeled on.
package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// passcodeRejectBody is the shape written by writeJSONErrorWithReason.
type passcodeRejectBody struct {
	Error  string `json:"error"`
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// startPasscodeTestServer builds and starts a terminal server with a token
// validator, a PasscodeVerifier that only accepts "correct-pin", and the
// given hasPasscode signal. Returns the port and a teardown func.
func startPasscodeTestServer(t *testing.T, port int, hasPasscode func() bool) (int, func()) {
	t.Helper()
	auth := NewMockTokenValidator()
	auth.AddValidToken("tok-1", &TokenInfo{UserID: "alice", OrgID: "org-1"})

	cfg := &Config{
		Host:           "127.0.0.1",
		Port:           port,
		MaxConnections: 10,
		IdleTimeout:    30 * time.Minute,
		OrgID:          "org-1",
		Shell:          "/bin/sh",
		RateLimitRPS:   100,
		RateLimitBurst: 100,
		PasscodeVerifier: func(pin string) bool {
			return pin == "correct-pin"
		},
	}
	if hasPasscode != nil {
		cfg.PasscodeHasPasscode = hasPasscode
	}

	s := NewServer(cfg, auth)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return port, func() { _ = s.Stop(context.Background()) }
}

func fetchPasscodeReject(t *testing.T, port int, passcode string) passcodeRejectBody {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/terminal?token=tok-1&passcode=%s", port, passcode)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	var body passcodeRejectBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode reject body: %v", err)
	}
	return body
}

// TestPasscodeReject_NotSet confirms that when PasscodeHasPasscode reports no
// passcode configured, the reject carries ReasonPasscodeNotSet regardless of
// what was presented.
func TestPasscodeReject_NotSet(t *testing.T) {
	port, stop := startPasscodeTestServer(t, 17901, func() bool { return false })
	defer stop()

	body := fetchPasscodeReject(t, port, "anything")
	if body.Reason != ReasonPasscodeNotSet {
		t.Errorf("reason = %q, want %q", body.Reason, ReasonPasscodeNotSet)
	}
}

// TestPasscodeReject_Invalid confirms that when a passcode IS configured but
// the wrong one is presented, the reject carries ReasonPasscodeInvalid.
func TestPasscodeReject_Invalid(t *testing.T) {
	port, stop := startPasscodeTestServer(t, 17902, func() bool { return true })
	defer stop()

	body := fetchPasscodeReject(t, port, "wrong-pin")
	if body.Reason != ReasonPasscodeInvalid {
		t.Errorf("reason = %q, want %q", body.Reason, ReasonPasscodeInvalid)
	}
}

// TestPasscodeReject_NoHasPasscodeWired confirms the pre-#753 fallback: when
// PasscodeHasPasscode is not wired at all (nil), the reject response omits
// the reason field rather than guessing, so callers that only wire
// PasscodeVerifier (e.g. any config predating this field) see unchanged
// behavior.
func TestPasscodeReject_NoHasPasscodeWired(t *testing.T) {
	port, stop := startPasscodeTestServer(t, 17903, nil)
	defer stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/terminal?token=tok-1&passcode=wrong", port)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode reject body: %v", err)
	}
	if _, present := raw["reason"]; present {
		t.Errorf("reason field present (%v) when PasscodeHasPasscode is nil; want omitted", raw["reason"])
	}
}

// TestPasscodeReject_CorrectPinSucceeds is a control: presenting the correct
// pin must not be rejected by the passcode gate at all (the test asserts the
// request proceeds past the gate to the WS upgrade, which fails this plain
// http.Get for lack of upgrade headers, a different status than the 401 the
// passcode gate would have produced, confirming the gate itself passed).
func TestPasscodeReject_CorrectPinSucceeds(t *testing.T) {
	port, stop := startPasscodeTestServer(t, 17904, func() bool { return true })
	defer stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/terminal?token=tok-1&passcode=correct-pin", port)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	// A non-WS GET past the passcode gate fails the Upgrade() call itself,
	// which is not the 401 the passcode gate would have produced. This
	// confirms the gate let it through.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("correct passcode was still rejected with 401")
	}
}
