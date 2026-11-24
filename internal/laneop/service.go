// Package laneop implements LANE_OPERATION against a tmux pane: a bounded,
// scrubbed capture and a guarded, once-per-offer paste-and-submit. All tmux
// access goes through the Exec seam, and every send is recorded in a durable
// node-local ledger before anything is typed.
package laneop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

const (
	MaxCaptureBytes = 8192
	MaxMessageBytes = 4096
	// HumanActivityWindow is how recent a key press on an attached client
	// must be for a send to refuse with human_driving.
	HumanActivityWindow = 60 * time.Second
	// DefaultSettle is the pause after the paste and after Enter before the
	// screen is read back.
	DefaultSettle = 400 * time.Millisecond
)

// Refusal reasons. The screen and human reasons are #1657's names.
const (
	ReasonHumanPromptOpen   = "human_prompt_open"
	ReasonHumanDraftPending = "human_draft_pending"
	ReasonHumanDriving      = "human_driving"
	ReasonUnknownScreen     = "unknown_screen"
	ReasonPaneNotFound      = "pane_not_found"
	ReasonOfferMismatch     = "offer_mismatch"
	ReasonLedgerFull        = "offer_ledger_full"
	ReasonTmuxUnavailable   = "tmux_unavailable"
)

// Server-side value patterns (aceteam utils/shell_dispatch.py), rechecked here.
var (
	PanePattern   = regexp.MustCompile(`^%[0-9]{1,12}$`)
	LanePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	OfferPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	sessionIDExpr = regexp.MustCompile(`^\$[0-9]{1,12}$`)
	activityExpr  = regexp.MustCompile(`^[0-9]{1,12}$`)
)

// Refusal is a definite non-action with a stable reason.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "lane operation refused: " + r.Reason }

// Service runs lane operations. Exec and Ledger are required; the rest have
// production defaults.
type Service struct {
	Exec   Exec
	Ledger *Ledger
	Now    func() time.Time
	Settle time.Duration
	Sleep  func(context.Context, time.Duration) error
	// Audit receives one line per send: lane, pane, offer and outcome only.
	Audit func(format string, args ...any)
	// sendMu serializes sends on this node, so two offers never interleave
	// keystrokes into one pane.
	sendMu sync.Mutex
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) settle(ctx context.Context) error {
	d := s.Settle
	if d < 0 {
		d = 0
	}
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	if d == 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type pane struct {
	id       string
	session  string
	dead     bool
	inMode   bool
	inputOff bool
	synced   bool
	managed  bool
}

const paneFormat = "#{pane_id}|#{session_id}|#{pane_dead}|#{pane_in_mode}|#{pane_input_off}|#{pane_synchronized}|#{?#{@citadel_managed},1,0}"

// resolvePane accepts only a pane listed by this user's own tmux server, by
// exact id, and never a Citadel-managed (human web terminal) session.
func (s *Service) resolvePane(ctx context.Context, id string) (pane, error) {
	if !PanePattern.MatchString(id) || s.Exec == nil {
		return pane{}, &Refusal{Reason: ReasonPaneNotFound}
	}
	listed, err := s.Exec(ctx, nil, "list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		return pane{}, &Refusal{Reason: ReasonTmuxUnavailable}
	}
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(string(listed), "\n"), "\n") {
		if line == id {
			found = true
		}
	}
	if !found {
		return pane{}, &Refusal{Reason: ReasonPaneNotFound}
	}
	raw, err := s.Exec(ctx, nil, "display-message", "-p", "-t", id, paneFormat)
	if err != nil {
		return pane{}, &Refusal{Reason: ReasonPaneNotFound}
	}
	fields := strings.Split(strings.TrimSuffix(string(raw), "\n"), "|")
	if len(fields) != 7 || fields[0] != id || !sessionIDExpr.MatchString(fields[1]) {
		return pane{}, &Refusal{Reason: ReasonPaneNotFound}
	}
	flags := make([]bool, 5)
	for i, value := range fields[2:] {
		switch value {
		case "0":
		case "1":
			flags[i] = true
		default:
			return pane{}, &Refusal{Reason: ReasonPaneNotFound}
		}
	}
	p := pane{id: id, session: fields[1], dead: flags[0], inMode: flags[1], inputOff: flags[2], synced: flags[3], managed: flags[4]}
	if p.managed {
		return pane{}, &Refusal{Reason: ReasonPaneNotFound}
	}
	return p, nil
}

func (s *Service) captureText(ctx context.Context, p pane) (string, error) {
	raw, err := s.Exec(ctx, nil, "capture-pane", "-p", "-J", "-t", p.id)
	if err != nil || !utf8.Valid(raw) {
		return "", &Refusal{Reason: ReasonTmuxUnavailable}
	}
	return string(raw), nil
}

// latestActivity is the newest key-press time of any client attached to the
// pane's session, or zero when none is attached.
func (s *Service) latestActivity(ctx context.Context, p pane) (time.Time, error) {
	raw, err := s.Exec(ctx, nil, "list-clients", "-t", p.session, "-F", "#{client_activity}")
	if err != nil {
		return time.Time{}, err
	}
	var latest int64
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !activityExpr.MatchString(line) {
			return time.Time{}, ErrTmuxUnavailable
		}
		v, _ := strconv.ParseInt(line, 10, 64)
		if v > latest {
			latest = v
		}
	}
	if latest == 0 {
		return time.Time{}, nil
	}
	return time.Unix(latest, 0), nil
}

// CaptureResult is the capture wire result (aceteam PR #13047).
type CaptureResult struct {
	Tail       string
	CapturedAt string
	Busy       *bool
	Evidence   string
}

// Map is the job output for a capture.
func (r CaptureResult) Map() map[string]any {
	out := map[string]any{"tail": r.Tail, "captured_at": r.CapturedAt, "busy_evidence": r.Evidence, "pane_busy": nil}
	if r.Busy != nil {
		out["pane_busy"] = *r.Busy
	}
	return out
}

// Capture returns at most maxBytes of the pane's visible screen, ending at its
// last line. Secrets are scrubbed over the whole capture before it is cut, so
// a cut can only drop text, never expose part of a credential. extra holds
// literal values that must also never leave the node (the job's passcode).
func (s *Service) Capture(ctx context.Context, paneID string, maxBytes int, extra ...string) (CaptureResult, error) {
	if maxBytes <= 0 || maxBytes > MaxCaptureBytes {
		return CaptureResult{}, &Refusal{Reason: "invalid_bound"}
	}
	p, err := s.resolvePane(ctx, paneID)
	if err != nil {
		return CaptureResult{}, err
	}
	text, err := s.captureText(ctx, p)
	if err != nil {
		return CaptureResult{}, err
	}
	capturedAt := s.now().UTC().Format(time.RFC3339)
	text = trimScreen(text)
	scrubbed := Scrub(text, extra...)
	tail := Scrub(tailBytes(scrubbed, maxBytes), extra...)
	result := CaptureResult{Tail: tailBytes(tail, maxBytes), CapturedAt: capturedAt, Evidence: "none"}
	screen := ReadScreen(text)
	if !p.dead && !p.inMode {
		switch {
		case screen.Busy:
			busy := true
			result.Busy, result.Evidence = &busy, "activity_recent"
		case screen.Kind == ScreenPrompt:
			busy := false
			result.Busy, result.Evidence = &busy, "prompt_visible"
		}
	}
	return result, nil
}

func trimScreen(text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// tailBytes returns the longest suffix of s that is at most n bytes and starts
// on a rune boundary.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// Scrub replaces every credential trust.CheckSecrets finds, and every literal
// in extra, with [REDACTED].
func Scrub(text string, extra ...string) string {
	for _, value := range extra {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	findings := trust.CheckSecrets("", text)
	if len(findings) == 0 {
		return text
	}
	spans := make([][2]int, 0, len(findings))
	for _, f := range findings {
		spans = append(spans, [2]int{f.Start, f.End})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	merged := spans[:1]
	for _, span := range spans[1:] {
		last := &merged[len(merged)-1]
		if span[0] <= last[1] {
			if span[1] > last[1] {
				last[1] = span[1]
			}
			continue
		}
		merged = append(merged, span)
	}
	var b strings.Builder
	pos := 0
	for _, span := range merged {
		b.WriteString(text[pos:span[0]])
		b.WriteString("[REDACTED]")
		pos = span[1]
	}
	b.WriteString(text[pos:])
	return b.String()
}

// SendRequest is one send. Message is literal text, already validated.
type SendRequest struct {
	Lane, LeaseID, NodeID, Pane, OfferID, Message string
}

// SendResult is the send wire result (aceteam PR #13047).
type SendResult struct {
	Status, Reason string
	Replayed       bool
}

// Map is the job output for a send.
func (r SendResult) Map(offerID string) map[string]any {
	out := map[string]any{"status": r.Status, "offer_id": offerID}
	if r.Reason != "" {
		out["reason"] = r.Reason
	}
	if r.Replayed {
		out["replayed"] = true
	}
	return out
}

// Fingerprint binds an offer to its exact destination and message bytes.
func Fingerprint(req SendRequest) string {
	digest := sha256.Sum256([]byte(req.Message))
	raw, _ := json.Marshal([]string{req.OfferID, req.Lane, req.LeaseID, req.NodeID, req.Pane, hex.EncodeToString(digest[:])})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ValidMessage mirrors the server's literal-message rule: non-empty UTF-8 of
// at most MaxMessageBytes, with no control character other than LF and tab.
func ValidMessage(message string) bool {
	if message == "" || len(message) > MaxMessageBytes || !utf8.ValidString(message) {
		return false
	}
	for _, r := range message {
		if (r < 32 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			return false
		}
	}
	return true
}

// Send submits req.Message to the pane at most once per offer. An offer that
// already has a record returns it without touching tmux; a pending record
// (an interrupted attempt) reads as submission_unknown. Nothing is typed
// unless the screen is a recognized, empty input box with no dialog, no copy
// mode and no recent key press from an attached client.
func (s *Service) Send(ctx context.Context, req SendRequest) SendResult {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	fp := Fingerprint(req)
	result := s.send(ctx, req, fp)
	s.audit(req, result)
	return result
}

func replay(entry Entry, fp string) SendResult {
	if entry.Fingerprint != fp {
		return SendResult{Status: StatusRejected, Reason: ReasonOfferMismatch}
	}
	status := entry.Status
	if status != StatusSubmitted && status != StatusRejected {
		status = StatusUnknown
	}
	return SendResult{Status: status, Reason: entry.Reason, Replayed: true}
}

func (s *Service) send(ctx context.Context, req SendRequest, fp string) SendResult {
	unknown := SendResult{Status: StatusUnknown}
	if s.Ledger == nil {
		return unknown
	}
	if prior, ok, err := s.Ledger.Lookup(req.OfferID); err != nil {
		return unknown
	} else if ok {
		return replay(prior, fp)
	}
	reject := func(reason string) SendResult {
		prior, existed, err := s.Ledger.Reserve(req.OfferID, Entry{Fingerprint: fp, Status: StatusRejected, Reason: reason}, s.now())
		switch {
		case errors.Is(err, ErrLedgerFull):
			return SendResult{Status: StatusRejected, Reason: ReasonLedgerFull}
		case err != nil:
			return unknown
		case existed:
			return replay(prior, fp)
		}
		return SendResult{Status: StatusRejected, Reason: reason}
	}
	p, err := s.resolvePane(ctx, req.Pane)
	if err != nil {
		return reject(refusalReason(err))
	}
	start := s.now()
	if reason := s.preflight(ctx, p, req.Message, start.Add(-HumanActivityWindow), true); reason != "" {
		return reject(reason)
	}
	prior, existed, err := s.Ledger.Reserve(req.OfferID, Entry{Fingerprint: fp, Status: statusPending}, start)
	switch {
	case errors.Is(err, ErrLedgerFull):
		return SendResult{Status: StatusRejected, Reason: ReasonLedgerFull}
	case err != nil:
		return unknown
	case existed:
		return replay(prior, fp)
	}
	outcome := s.submit(ctx, p, req.Message, start)
	if err := s.Ledger.Settle(req.OfferID, fp, outcome.Status, outcome.Reason, s.now()); err != nil {
		// The pending record stays, and it already reads as unknown.
		if outcome.Status == StatusSubmitted {
			return outcome
		}
		return unknown
	}
	return outcome
}

// preflight returns the refusal reason for the pane's current state, or "".
// wantEmpty selects the pre-paste check (an empty box) over the post-paste
// check (the box holds exactly message).
func (s *Service) preflight(ctx context.Context, p pane, message string, since time.Time, wantEmpty bool) string {
	if p.dead || p.inputOff || p.synced {
		return ReasonUnknownScreen
	}
	if p.inMode {
		return ReasonHumanDriving
	}
	latest, err := s.latestActivity(ctx, p)
	if err != nil {
		return ReasonUnknownScreen
	}
	if !latest.IsZero() && !latest.Before(since.Truncate(time.Second)) {
		return ReasonHumanDriving
	}
	text, err := s.captureText(ctx, p)
	if err != nil {
		return ReasonUnknownScreen
	}
	screen := ReadScreen(text)
	switch {
	case screen.Kind == ScreenDialog:
		return ReasonHumanPromptOpen
	case screen.Kind != ScreenPrompt:
		return ReasonUnknownScreen
	case wantEmpty && screen.Draft != "":
		return ReasonHumanDraftPending
	case !wantEmpty && !draftHolds(screen.Draft, message):
		return ReasonUnknownScreen
	}
	return ""
}

func (s *Service) submit(ctx context.Context, p pane, message string, start time.Time) SendResult {
	unknown := SendResult{Status: StatusUnknown}
	buffer, err := bufferName()
	if err != nil {
		return SendResult{Status: StatusRejected, Reason: ReasonTmuxUnavailable}
	}
	if _, err := s.Exec(ctx, []byte(message), "load-buffer", "-b", buffer, "-"); err != nil {
		// Nothing reached the pane; only the server's buffer may hold it.
		_, _ = s.Exec(context.WithoutCancel(ctx), nil, "delete-buffer", "-b", buffer)
		return SendResult{Status: StatusRejected, Reason: ReasonTmuxUnavailable}
	}
	// -p wraps the text in bracketed-paste markers when the harness asked for
	// them, and -r keeps LF as LF, so a newline inside the message is text,
	// never an extra submit. -d deletes the buffer once pasted.
	if _, err := s.Exec(ctx, nil, "paste-buffer", "-d", "-p", "-r", "-b", buffer, "-t", p.id); err != nil {
		_, _ = s.Exec(context.WithoutCancel(ctx), nil, "delete-buffer", "-b", buffer)
		return unknown
	}
	if s.settle(ctx) != nil {
		return unknown
	}
	again, err := s.resolvePane(ctx, p.id)
	if err != nil || again.session != p.session {
		return unknown
	}
	// Any key press since this send began means a human is in the box with
	// the pasted text: never press Enter on their behalf.
	if reason := s.preflight(ctx, again, message, start, false); reason != "" {
		return unknown
	}
	if _, err := s.Exec(ctx, nil, "send-keys", "-t", p.id, "Enter"); err != nil {
		return unknown
	}
	if s.settle(ctx) != nil {
		return unknown
	}
	text, err := s.captureText(ctx, again)
	if err != nil {
		return unknown
	}
	if screen := ReadScreen(text); screen.Kind == ScreenPrompt && screen.Draft == "" {
		return SendResult{Status: StatusSubmitted}
	}
	return unknown
}

func bufferName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "citadel-lane-" + hex.EncodeToString(b[:]), nil
}

func refusalReason(err error) string {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ReasonTmuxUnavailable
}

func (s *Service) audit(req SendRequest, result SendResult) {
	if s.Audit == nil {
		return
	}
	reason := result.Reason
	if reason == "" {
		reason = "-"
	}
	s.Audit("[lane] audit send lane=%s pane=%s offer=%s outcome=%s reason=%s replayed=%t",
		req.Lane, req.Pane, req.OfferID, result.Status, reason, result.Replayed)
}
