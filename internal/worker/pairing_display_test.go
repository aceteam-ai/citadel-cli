package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/pairingdisplay"
)

// fakePairingOps is an injected PairingDisplayOps for hermetic handler tests
// — no real console, no pairingdisplay.Get() singleton involved.
type fakePairingOps struct {
	showResult  pairingdisplay.ShowOutcome
	clearResult pairingdisplay.ClearOutcome

	showCalls  []fakeShowCall
	clearCalls []string
}

type fakeShowCall struct {
	code           string
	ttl            time.Duration
	grantRequestID string
	requestedBy    string
}

func (f *fakePairingOps) Show(code string, ttl time.Duration, grantRequestID, requestedBy string) pairingdisplay.ShowOutcome {
	f.showCalls = append(f.showCalls, fakeShowCall{code: code, ttl: ttl, grantRequestID: grantRequestID, requestedBy: requestedBy})
	return f.showResult
}

func (f *fakePairingOps) Clear(grantRequestID string) pairingdisplay.ClearOutcome {
	f.clearCalls = append(f.clearCalls, grantRequestID)
	return f.clearResult
}

// pairingOrgPoolQueue is a representative shared org-pool queue (no ":node:"
// segment) -- the queue the privilege gate must refuse. perNodeQueue (the
// per-node stream this handler must accept) is already defined as a
// package-level const in agent_update_test.go.
const pairingOrgPoolQueue = "jobs:v1:shell:org_1"

func TestPairingDisplayHandler_CanHandle(t *testing.T) {
	h := NewPairingDisplayHandler(PairingDisplayConfig{})
	if !h.CanHandle(JobTypeShowPairingCode) {
		t.Fatalf("expected CanHandle(SHOW_PAIRING_CODE) true")
	}
	if !h.CanHandle(JobTypeClearPairingCode) {
		t.Fatalf("expected CanHandle(CLEAR_PAIRING_CODE) true")
	}
	if h.CanHandle("SOMETHING_ELSE") {
		t.Fatalf("expected CanHandle for an unrelated type to be false")
	}
}

func TestPairingDisplayHandler_ShowRefusesOrgPoolQueue(t *testing.T) {
	ops := &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})

	job := &Job{Type: JobTypeShowPairingCode, SourceQueue: pairingOrgPoolQueue, Payload: map[string]any{
		"code": "12345678", "grant_request_id": "gr_1",
	}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("expected failure status for a non-per-node queue, got %v", res.Status)
	}
	if len(ops.showCalls) != 0 {
		t.Fatalf("Ops.Show must not be called when the privilege gate refuses, got %d calls", len(ops.showCalls))
	}
	if reason, _ := res.Output["reason"].(string); reason != "forbidden" {
		t.Fatalf("expected reason=forbidden, got %v", res.Output["reason"])
	}
}

func TestPairingDisplayHandler_ClearRefusesOrgPoolQueue(t *testing.T) {
	ops := &fakePairingOps{clearResult: pairingdisplay.ClearOutcome{Cleared: true}}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})

	job := &Job{Type: JobTypeClearPairingCode, SourceQueue: pairingOrgPoolQueue, Payload: map[string]any{
		"grant_request_id": "gr_1",
	}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure {
		t.Fatalf("expected failure status for a non-per-node queue, got %v", res.Status)
	}
	if len(ops.clearCalls) != 0 {
		t.Fatalf("Ops.Clear must not be called when the privilege gate refuses, got %d calls", len(ops.clearCalls))
	}
}

func TestPairingDisplayHandler_ShowSuccess(t *testing.T) {
	ops := &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})

	job := &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
		"code": "12345678", "grant_request_id": "gr_1", "requested_by": "Agent Ops",
	}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess {
		t.Fatalf("expected success status, got %v: %v", res.Status, res.Error)
	}
	if res.Output["delivered"] != true {
		t.Fatalf("expected delivered=true, got %v", res.Output)
	}
	if res.Output["surface"] != "console" {
		t.Fatalf("expected surface=console, got %v", res.Output)
	}
	if _, hasReason := res.Output["reason"]; hasReason {
		t.Fatalf("expected no reason key on a delivered result, got %v", res.Output)
	}
	if len(ops.showCalls) != 1 {
		t.Fatalf("expected 1 Show call, got %d", len(ops.showCalls))
	}
	call := ops.showCalls[0]
	if call.code != "12345678" || call.grantRequestID != "gr_1" || call.requestedBy != "Agent Ops" {
		t.Fatalf("unexpected show call: %+v", call)
	}
	if call.ttl != pairingTTLDefaultSeconds*time.Second {
		t.Fatalf("expected default ttl of %ds, got %v", pairingTTLDefaultSeconds, call.ttl)
	}
}

func TestPairingDisplayHandler_ShowTTLClamping(t *testing.T) {
	cases := []struct {
		name     string
		in       int
		expected int
	}{
		{"absent-defaults", 0, pairingTTLDefaultSeconds},
		{"too-low-clamped", 5, pairingTTLMinSeconds},
		{"too-high-clamped", 999999, pairingTTLMaxSeconds},
		{"in-range-preserved", 120, 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}}
			h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})
			job := &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
				"code": "12345678", "grant_request_id": "gr_1", "ttl_seconds": tc.in,
			}}
			if _, err := h.Execute(nil, job, nil); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(ops.showCalls) != 1 {
				t.Fatalf("expected 1 Show call, got %d", len(ops.showCalls))
			}
			got := ops.showCalls[0].ttl
			want := time.Duration(tc.expected) * time.Second
			if got != want {
				t.Fatalf("ttl_seconds=%d: expected clamped ttl %v, got %v", tc.in, want, got)
			}
		})
	}
}

func TestPairingDisplayHandler_ShowNotDelivered(t *testing.T) {
	ops := &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Reason: "graphical_session"}}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})

	job := &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
		"code": "12345678", "grant_request_id": "gr_1",
	}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// A "console not usable" outcome is a legitimate business answer, not a
	// technical job failure -- the backend needs delivered=false to fall
	// through to its linked-device chain, but the JOB itself succeeded.
	if res.Status != JobStatusSuccess {
		t.Fatalf("expected success status even when not delivered, got %v", res.Status)
	}
	if res.Output["delivered"] != false {
		t.Fatalf("expected delivered=false, got %v", res.Output)
	}
	if res.Output["reason"] != "graphical_session" {
		t.Fatalf("expected reason=graphical_session, got %v", res.Output)
	}
}

func TestPairingDisplayHandler_ShowBadPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"missing code", map[string]any{"grant_request_id": "gr_1"}},
		{"missing grant_request_id", map[string]any{"code": "12345678"}},
		{"code too long", map[string]any{"code": strings.Repeat("9", pairingCodeMaxLen+1), "grant_request_id": "gr_1"}},
		{"code non-ascii", map[string]any{"code": "code\x01bad", "grant_request_id": "gr_1"}},
		{"nil payload", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}}
			h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})
			job := &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: tc.payload}
			res, err := h.Execute(nil, job, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Status != JobStatusFailure {
				t.Fatalf("expected failure status, got %v", res.Status)
			}
			if res.Output["reason"] != "bad_payload" {
				t.Fatalf("expected reason=bad_payload, got %v", res.Output)
			}
			if len(ops.showCalls) != 0 {
				t.Fatalf("Ops.Show must not be called on a bad payload, got %d calls", len(ops.showCalls))
			}
		})
	}
}

// TestPairingDisplayHandler_ShowSanitizesRequestedBy pins the review-caught
// fail-closed fix: requested_by is free text, bounded but (before this fix)
// never charset-validated, unlike code. Because it is rendered directly
// into the console frame, an embedded ANSI/control sequence could erase an
// already-rendered code while Show still reports delivered:true -- the
// exact false-positive the package's governing rule forbids. This asserts
// the sanitized value (control bytes, including ESC, stripped) is what
// actually reaches Ops.Show -- i.e. what the render layer receives -- and
// that the code itself is untouched.
func TestPairingDisplayHandler_ShowSanitizesRequestedBy(t *testing.T) {
	ops := &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})

	const maliciousRequestedBy = "evil\x1b[2J\x1b[Hname"
	job := &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
		"code": "12345678", "grant_request_id": "gr_1", "requested_by": maliciousRequestedBy,
	}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess || res.Output["delivered"] != true {
		t.Fatalf("expected a successful delivery, got %v %v", res.Status, res.Output)
	}
	if len(ops.showCalls) != 1 {
		t.Fatalf("expected 1 Show call, got %d", len(ops.showCalls))
	}
	got := ops.showCalls[0].requestedBy
	if strings.Contains(got, "\x1b") {
		t.Fatalf("expected the ESC byte to be stripped from requested_by before it reaches the render layer, got %q", got)
	}
	if got != "evil[2J[Hname" {
		t.Fatalf("expected the sanitized (ESC-stripped) requested_by, got %q", got)
	}
	// The code itself is untouched by sanitization -- it went through
	// isPrintableASCII validation instead (rejected outright, not sanitized).
	if ops.showCalls[0].code != "12345678" {
		t.Fatalf("expected the code to be passed through unchanged, got %q", ops.showCalls[0].code)
	}
}

func TestPairingDisplayHandler_ClearSuccess(t *testing.T) {
	ops := &fakePairingOps{clearResult: pairingdisplay.ClearOutcome{Cleared: true}}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})
	job := &Job{Type: JobTypeClearPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
		"grant_request_id": "gr_1",
	}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusSuccess || res.Output["cleared"] != true {
		t.Fatalf("expected success/cleared=true, got %v %v", res.Status, res.Output)
	}
}

func TestPairingDisplayHandler_ClearNotDisplayedIsSuccess(t *testing.T) {
	ops := &fakePairingOps{clearResult: pairingdisplay.ClearOutcome{Reason: "not_displayed"}}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})
	job := &Job{Type: JobTypeClearPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
		"grant_request_id": "gr_expired",
	}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Idempotent success, never a job failure (design doc §8.2).
	if res.Status != JobStatusSuccess {
		t.Fatalf("expected success status for an already-cleared code, got %v", res.Status)
	}
	if res.Output["cleared"] != false || res.Output["reason"] != "not_displayed" {
		t.Fatalf("unexpected output: %v", res.Output)
	}
}

func TestPairingDisplayHandler_ClearBadPayload(t *testing.T) {
	ops := &fakePairingOps{}
	h := NewPairingDisplayHandler(PairingDisplayConfig{Ops: ops})
	job := &Job{Type: JobTypeClearPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{}}
	res, err := h.Execute(nil, job, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != JobStatusFailure || res.Output["reason"] != "bad_payload" {
		t.Fatalf("expected failure/bad_payload, got %v %v", res.Status, res.Output)
	}
	if len(ops.clearCalls) != 0 {
		t.Fatalf("Ops.Clear must not be called on a bad payload, got %d calls", len(ops.clearCalls))
	}
}

// TestPairingDisplayHandler_SentinelNeverLeaks is the load-bearing security
// test (design doc §10.3): the code must never appear in the job result, the
// error text, or any captured log line, across every branch of the handler
// -- including every failure branch, where a careless "log the payload for
// debugging" edit is most likely to be added later.
func TestPairingDisplayHandler_SentinelNeverLeaks(t *testing.T) {
	const sentinel = "SENTINEL-PAIRING-CODE-7f3a9c21"

	var loggedLines []string
	logFn := func(format string, args ...any) {
		loggedLines = append(loggedLines, fmt.Sprintf(format, args...))
	}

	type attempt struct {
		name string
		ops  *fakePairingOps
		job  *Job
	}

	attempts := []attempt{
		{
			name: "delivered",
			ops:  &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}},
			job: &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
				"code": sentinel, "grant_request_id": "gr_1", "requested_by": sentinel,
			}},
		},
		{
			name: "not delivered - graphical session",
			ops:  &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Reason: "graphical_session"}},
			job: &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
				"code": sentinel, "grant_request_id": "gr_2",
			}},
		},
		{
			name: "not delivered - permission denied",
			ops:  &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Reason: "permission_denied"}},
			job: &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
				"code": sentinel, "grant_request_id": "gr_3",
			}},
		},
		{
			name: "gate refused (org pool queue)",
			ops:  &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}},
			job: &Job{Type: JobTypeShowPairingCode, SourceQueue: pairingOrgPoolQueue, Payload: map[string]any{
				"code": sentinel, "grant_request_id": "gr_4",
			}},
		},
		{
			name: "bad payload - code too long (sentinel embedded)",
			ops:  &fakePairingOps{},
			job: &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
				"code": sentinel + sentinel, "grant_request_id": "gr_5",
			}},
		},
		{
			name: "misconfigured ops (nil)",
			ops:  nil,
			job: &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
				"code": sentinel, "grant_request_id": "gr_6",
			}},
		},
		{
			// ANSI/control bytes in requested_by alongside a sentinel code:
			// the review-caught case. Even though requested_by is sanitized
			// (not rejected), this confirms sanitization does not somehow
			// route the CODE itself into a log line or result field.
			name: "requested_by carries ANSI/control bytes",
			ops:  &fakePairingOps{showResult: pairingdisplay.ShowOutcome{Delivered: true, Surface: "console"}},
			job: &Job{Type: JobTypeShowPairingCode, SourceQueue: perNodeQueue, Payload: map[string]any{
				"code": sentinel, "grant_request_id": "gr_7", "requested_by": "\x1b[2J\x1b[H" + sentinel,
			}},
		},
	}

	for _, a := range attempts {
		t.Run(a.name, func(t *testing.T) {
			loggedLines = nil
			var h *PairingDisplayHandler
			if a.ops != nil {
				h = NewPairingDisplayHandler(PairingDisplayConfig{Ops: a.ops, Log: logFn})
			} else {
				h = NewPairingDisplayHandler(PairingDisplayConfig{Log: logFn})
			}

			res, err := h.Execute(nil, a.job, nil)
			if err != nil {
				t.Fatalf("unexpected transport error: %v", err)
			}

			assertNoSentinel(t, sentinel, "job result output", mustMarshal(t, res.Output))
			if res.Error != nil {
				assertNoSentinel(t, sentinel, "job result error", res.Error.Error())
			}
			for i, line := range loggedLines {
				assertNoSentinel(t, sentinel, fmt.Sprintf("log line %d", i), line)
			}
		})
	}
}

func assertNoSentinel(t *testing.T, sentinel, what, text string) {
	t.Helper()
	if strings.Contains(text, sentinel) {
		t.Fatalf("SECURITY: %s leaked the pairing code sentinel: %q", what, text)
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
