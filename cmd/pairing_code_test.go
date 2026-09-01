package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/pairingdisplay"
)

func TestRenderPairingCode_PendingUnexpired_HumanFormat(t *testing.T) {
	var buf bytes.Buffer
	info := pairingdisplay.PendingCodeInfo{
		Pending:        true,
		Code:           "12345678",
		GrantRequestID: "gr_1",
		RequestedBy:    "Agent Ops for jane@example.com",
		ExpiresAt:      time.Now().Add(5 * time.Minute),
		TTLSeconds:     300,
	}

	if err := renderPairingCode(&buf, info, false); err != nil {
		t.Fatalf("renderPairingCode: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "12345678") {
		t.Fatalf("expected the code in the output, got %q", out)
	}
	if !strings.Contains(out, "Agent Ops for jane@example.com") {
		t.Fatalf("expected requested_by in the output, got %q", out)
	}
	if strings.Contains(out, "No pending pairing code") {
		t.Fatalf("did not expect the no-pending message, got %q", out)
	}
}

func TestRenderPairingCode_NonePending_HumanFormat(t *testing.T) {
	var buf bytes.Buffer
	info := pairingdisplay.PendingCodeInfo{Pending: false}

	if err := renderPairingCode(&buf, info, false); err != nil {
		t.Fatalf("renderPairingCode: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "No pending pairing code.") {
		t.Fatalf("expected the no-pending message, got %q", out)
	}
	if strings.Contains(out, "Code:") {
		t.Fatalf("did not expect a Code: line when nothing is pending, got %q", out)
	}
}

func TestRenderPairingCode_ExpiredTreatedAsNone(t *testing.T) {
	// A caller (RequestPendingCode) is contractually responsible for never
	// handing renderPairingCode an expired Pending=true snapshot (both the
	// server-side snapshotPending and the client-side RequestPendingCode
	// re-check the TTL defensively) -- but renderPairingCode itself only
	// ever receives what RequestPendingCode already normalized. This test
	// pins the CALLER contract: an info struct representing "nothing
	// pending" (the shape RequestPendingCode returns for an expired code)
	// renders identically to "none ever was".
	var buf bytes.Buffer
	info := pairingdisplay.PendingCodeInfo{} // zero value: what an expired/absent code normalizes to

	if err := renderPairingCode(&buf, info, false); err != nil {
		t.Fatalf("renderPairingCode: %v", err)
	}
	if !strings.Contains(buf.String(), "No pending pairing code.") {
		t.Fatalf("expected the no-pending message for an expired/absent code, got %q", buf.String())
	}
}

func TestRenderPairingCode_JSONShape_Pending(t *testing.T) {
	var buf bytes.Buffer
	expiresAt := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	info := pairingdisplay.PendingCodeInfo{
		Pending:        true,
		Code:           "12345678",
		GrantRequestID: "gr_1",
		RequestedBy:    "someone",
		ExpiresAt:      expiresAt,
		TTLSeconds:     300,
	}

	if err := renderPairingCode(&buf, info, true); err != nil {
		t.Fatalf("renderPairingCode: %v", err)
	}

	var decoded pairingdisplay.PendingCodeInfo
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if !decoded.Pending || decoded.Code != "12345678" || decoded.GrantRequestID != "gr_1" {
		t.Fatalf("decoded JSON does not match input: %+v", decoded)
	}
	if !decoded.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expected expires_at %v, got %v", expiresAt, decoded.ExpiresAt)
	}
}

func TestRenderPairingCode_JSONShape_NonePending(t *testing.T) {
	var buf bytes.Buffer
	info := pairingdisplay.PendingCodeInfo{}

	if err := renderPairingCode(&buf, info, true); err != nil {
		t.Fatalf("renderPairingCode: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if pending, ok := decoded["pending"].(bool); !ok || pending {
		t.Fatalf(`expected "pending": false, got %v`, decoded["pending"])
	}
	// omitempty fields must be absent, not present-and-zero -- so a script
	// checking for the KEY's presence (not just its value) behaves.
	for _, field := range []string{"code", "grant_request_id", "requested_by"} {
		if _, present := decoded[field]; present {
			t.Fatalf("expected field %q to be omitted when nothing is pending, got %+v", field, decoded)
		}
	}
}

func TestRenderPairingCodeError_JSONShape(t *testing.T) {
	// Pins the review-mandated contract: a RequestPendingCode failure in
	// --json mode (e.g. pairingdisplay.ErrUnsupportedPlatform on Windows)
	// must still produce a valid JSON body on stdout, not a bare Go error
	// string -- so a script parsing --json output never gets a broken
	// pipe. renderPairingCodeError must also return the error UNCHANGED, so
	// the command still exits non-zero.
	var buf bytes.Buffer
	origErr := errors.New("read pending pairing code: " + pairingdisplay.ErrUnsupportedPlatform.Error())

	returned := renderPairingCodeError(&buf, origErr)
	if returned != origErr {
		t.Fatalf("expected renderPairingCodeError to return the same error unchanged, got %v", returned)
	}

	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if pending, ok := decoded["pending"].(bool); !ok || pending {
		t.Fatalf(`expected "pending": false, got %v`, decoded["pending"])
	}
	errMsg, ok := decoded["error"].(string)
	if !ok || !strings.Contains(errMsg, "not supported on this platform") {
		t.Fatalf("expected an explanatory error message in the JSON body, got %+v", decoded)
	}
}

func TestFormatRemainingTTL(t *testing.T) {
	cases := []struct {
		seconds int
		want    string
	}{
		{0, "less than a second"},
		{-5, "less than a second"},
		{30, "30s"},
		{90, "1m30s"},
		{600, "10m0s"},
	}
	for _, tc := range cases {
		if got := formatRemainingTTL(tc.seconds); got != tc.want {
			t.Errorf("formatRemainingTTL(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}
