package jobs

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMeetPage is a scripted meetPage for the interactive loop. It serves chat
// reads from a queue (advancing one entry per read, then repeating the last),
// answers the end heuristics from flags, and records whether the leave click ran.
type fakeMeetPage struct {
	mu sync.Mutex

	chatReads   []string // JSON strings returned by successive chat reads
	chatIdx     int
	ended       bool // meetIsEndedJS result
	participant any  // meetParticipantCountJS result (nil -> -1)

	openErr  error
	postErr  error
	leaveHit bool
	posted   []string
}

func (f *fakeMeetPage) Evaluate(expression string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case expression == meetLeaveCallJS:
		f.leaveHit = true
		return true, nil
	case expression == meetIsEndedJS:
		return f.ended, nil
	case expression == meetParticipantCountJS:
		if f.participant == nil {
			return float64(-1), nil
		}
		return f.participant, nil
	case strings.Contains(expression, "JSON.stringify"): // chat read
		var v string
		if f.chatIdx < len(f.chatReads) {
			v = f.chatReads[f.chatIdx]
			f.chatIdx++
		} else if len(f.chatReads) > 0 {
			v = f.chatReads[len(f.chatReads)-1]
		} else {
			v = "[]"
		}
		return v, nil
	case strings.Contains(expression, "chat input not found"): // post
		f.posted = append(f.posted, expression)
		return true, f.postErr
	default: // open chat
		return true, f.openErr
	}
}

func newTestInteractiveHandler() *MeetingJoinHandler {
	h := NewMeetingJoinHandler("/ws")
	h.StreamingEnabled = true
	h.StreamingInterval = time.Millisecond // fast rolling ticks
	// A small positive window is honored (0 would fall back to the default). The
	// stability margin always withholds the churning tail segment, so tests that
	// exercise a streamed command supply a trailing segment past the margin.
	h.StreamingWindow = time.Second
	return h
}

func testParams() meetingJoinParams {
	return meetingJoinParams{MeetingURL: "https://meet.google.com/x", MeetingID: "m1", MaxDurationSeconds: 3600}
}

func TestInteractive_LeaveViaChat(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{chatReads: []string{`[{"index":0,"sender":"Bob","text":"/ace leave"}]`}}
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{})

	if out.endReason != "ace_command_leave" {
		t.Errorf("endReason = %q, want ace_command_leave", out.endReason)
	}
	if !page.leaveHit {
		t.Error("expected the graceful leave click to have run")
	}
	if len(out.chat) != 1 || out.chat[0].Text != "/ace leave" {
		t.Errorf("captured chat = %+v, want the leave line", out.chat)
	}
	if len(out.recognized) != 1 || out.recognized[0].Kind != CommandLeave {
		t.Errorf("recognized = %+v, want one leave command", out.recognized)
	}
}

func TestInteractive_LeaveViaSpokenTranscript(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{} // empty chat, never ends on its own
	// The rolling transcriber surfaces the command segment plus a later trailing
	// segment, so the command is past the stability margin (the tail is withheld).
	transcribe := func() ([]TranscriptSegment, error) {
		return []TranscriptSegment{
			{Start: 0, End: 2, Text: "okay, /ace leave everyone"},
			{Start: 2, End: 20, Text: "bye now"},
		}, nil
	}

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), transcribe, time.Millisecond, map[string]struct{}{})

	if out.endReason != "ace_command_leave" {
		t.Errorf("endReason = %q, want ace_command_leave", out.endReason)
	}
	if out.streamedSegments == 0 {
		t.Error("expected at least one streamed segment to have been processed")
	}
}

// TestInteractive_RollingStopsWhenCallEnds is the Bug B lifecycle guard at the
// loop level (live-prod node 1084, 2026-07-16). Even when EVERY rolling pass
// fails (simulating the Bug A perms cascade where the scratch clip + whole-file
// read were both denied), the interactive loop must (1) return promptly with a
// terminal endReason, and (2) fully stop the rolling goroutine before returning
// — so Execute proceeds to the batch transcribe and no pass keeps firing after
// the call ended.
func TestInteractive_RollingStopsWhenCallEnds(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{ended: true} // ends on the first end-check
	var calls int32
	failingTranscribe := func() ([]TranscriptSegment, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("whisper unreachable (simulated Bug A cascade)")
	}

	done := make(chan interactiveOutcome, 1)
	go func() {
		done <- h.waitForMeetingEndInteractive(
			JobContext{}, page, testParams(), failingTranscribe, time.Millisecond, map[string]struct{}{},
		)
	}()

	var out interactiveOutcome
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForMeetingEndInteractive did not return; rolling loop likely still firing after the call ended")
	}
	if out.endReason != "call_ended" {
		t.Errorf("endReason = %q, want call_ended (terminal outcome despite all rolling passes failing)", out.endReason)
	}

	// The deferred stopRolling joined the goroutine before the function returned,
	// so no further rolling passes may run. Snapshot, wait past many tick
	// intervals (StreamingInterval is 1ms), and re-check the count is frozen.
	before := atomic.LoadInt32(&calls)
	time.Sleep(50 * time.Millisecond)
	if after := atomic.LoadInt32(&calls); after != before {
		t.Errorf("rolling passes fired AFTER the loop returned: before=%d after=%d (goroutine not stopped)", before, after)
	}
}

func TestInteractive_EndsWhenCallEnds(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{ended: true}
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{})
	if out.endReason != "call_ended" {
		t.Errorf("endReason = %q, want call_ended", out.endReason)
	}
	if page.leaveHit {
		t.Error("leave click must not run when the call ends on its own")
	}
}

func TestInteractive_AloneInCall(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{participant: float64(1)}
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{})
	if out.endReason != "alone_in_call" {
		t.Errorf("endReason = %q, want alone_in_call", out.endReason)
	}
}

func TestInteractive_CapturesGenericCommandsWithoutLeaving(t *testing.T) {
	h := newTestInteractiveHandler()
	// First read surfaces a generic command; the call then ends so the loop
	// terminates without an /ace leave.
	page := &fakeMeetPage{
		chatReads: []string{
			`[{"index":0,"sender":"Ann","text":"/ace summarize so far"}]`,
			`[{"index":0,"sender":"Ann","text":"/ace summarize so far"}]`,
		},
	}
	// End the call on the second poll.
	go func() {
		time.Sleep(20 * time.Millisecond)
		page.mu.Lock()
		page.ended = true
		page.mu.Unlock()
	}()
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{})

	if page.leaveHit {
		t.Error("a generic (non-leave) command must NOT trigger a leave")
	}
	if len(out.recognized) == 0 || out.recognized[0].Kind != CommandGeneric {
		t.Errorf("recognized = %+v, want a captured generic command", out.recognized)
	}
	if out.endReason != "call_ended" {
		t.Errorf("endReason = %q, want call_ended", out.endReason)
	}
}

func TestInteractive_ChatDedupByIndex(t *testing.T) {
	h := newTestInteractiveHandler()
	// The same message is returned on every read; it must be captured once.
	page := &fakeMeetPage{chatReads: []string{`[{"index":0,"sender":"Ann","text":"hello"}]`}}
	go func() {
		time.Sleep(15 * time.Millisecond)
		page.mu.Lock()
		page.ended = true
		page.mu.Unlock()
	}()
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{})
	if len(out.chat) != 1 {
		t.Errorf("captured chat = %d messages, want 1 (deduped by index across polls)", len(out.chat))
	}
}

func TestInteractive_TranscribeErrorIsNonFatal(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{ended: true}
	panicky := func() ([]TranscriptSegment, error) {
		panic("whisper exploded")
	}
	// A panic in the rolling pass is recovered on its goroutine; the loop still
	// ends the call normally.
	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), panicky, time.Millisecond, map[string]struct{}{})
	if out.endReason != "call_ended" {
		t.Errorf("endReason = %q, want call_ended despite a panicking transcriber", out.endReason)
	}
}

func TestAnnounceOnAdmission_PostsAnnouncement(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{}
	h.announceOnAdmission(JobContext{}, page, map[string]struct{}{})
	if len(page.posted) != 1 {
		t.Fatalf("expected exactly one chat post (the announcement), got %d", len(page.posted))
	}
	// The announcement text must be embedded (json-escaped) in the send JS.
	if !strings.Contains(page.posted[0], "recording this call") {
		t.Errorf("posted JS did not carry the announcement text; got: %s", page.posted[0])
	}
}

func TestAnnounceOnAdmission_SeedsOwnMessageGuard(t *testing.T) {
	h := newTestInteractiveHandler()
	botMessages := map[string]struct{}{}
	// Even when posting throws (unverified selectors), the guard must be seeded so
	// echo-suppression is in place BEFORE the post selectors are live-tuned.
	page := &fakeMeetPage{postErr: errors.New("no chat input yet")}
	h.announceOnAdmission(JobContext{}, page, botMessages)
	if _, ok := botMessages[normalizeChatText(meetAnnouncementText)]; !ok {
		t.Error("announceOnAdmission must seed the announcement into botMessages even when the post fails")
	}
}

// TestInteractive_HelpCommandDoesNotSelfLeaveOnEcho is the self-echo regression
// guard for the new verbs. A participant types `/ace help`; the bot posts the
// help text, which literally contains "/ace leave". Meet echoes the bot's own
// message back into the chat panel. If the loop scanned that echo it would parse
// the embedded "/ace leave" and end the call. The post-time seed of botMessages
// (done by the loop's postChat wrapper) must suppress command parsing for the
// echoed line while still capturing it for the audit trail.
func TestInteractive_HelpCommandDoesNotSelfLeaveOnEcho(t *testing.T) {
	h := newTestInteractiveHandler()

	helpJSON, _ := json.Marshal(meetingHelpText)
	page := &fakeMeetPage{chatReads: []string{
		// Poll 1: a participant asks for help (triggers the bot's help post).
		`[{"index":0,"sender":"Bob","text":"/ace help"}]`,
		// Poll 2: the bot's own help output is echoed back by Meet.
		`[{"index":0,"sender":"Bob","text":"/ace help"},{"index":1,"sender":"You","text":` + string(helpJSON) + `}]`,
	}}
	go func() {
		time.Sleep(25 * time.Millisecond)
		page.mu.Lock()
		page.ended = true
		page.mu.Unlock()
	}()
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{})

	if out.endReason == "ace_command_leave" || page.leaveHit {
		t.Fatal("bot self-triggered a leave from its own /ace help echo")
	}
	// Exactly one command recognized: Bob's /ace help. The echoed help line
	// (which contains "/ace leave") must be suppressed by the own-message guard.
	if len(out.recognized) != 1 || out.recognized[0].Kind != CommandHelp {
		t.Errorf("recognized = %+v, want exactly one help command", out.recognized)
	}
	// The bot must have actually posted the help text (recorded by the fake).
	var postedHelp bool
	for _, js := range page.posted {
		if strings.Contains(js, "AceTeam commands") {
			postedHelp = true
		}
	}
	if !postedHelp {
		t.Error("expected the bot to post the help text to chat")
	}
}

// TestInteractive_NoteCommandRecordsToOutcome verifies an in-call `/ace note`
// flows through the loop into the outcome's notes buffer (surfaced in the
// MEETING_JOIN result) and does not trigger a leave.
func TestInteractive_NoteCommandRecordsToOutcome(t *testing.T) {
	h := newTestInteractiveHandler()
	page := &fakeMeetPage{chatReads: []string{
		`[{"index":0,"sender":"Ann","text":"/ace note ship the release"}]`,
	}}
	go func() {
		time.Sleep(20 * time.Millisecond)
		page.mu.Lock()
		page.ended = true
		page.mu.Unlock()
	}()
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, map[string]struct{}{})

	if page.leaveHit {
		t.Error("/ace note must not trigger a leave")
	}
	if len(out.notes) != 1 || !strings.Contains(out.notes[0], "ship the release") {
		t.Errorf("outcome notes = %v, want the noted text", out.notes)
	}
}

// TestInteractive_DoesNotSelfTriggerOnAnnouncementEcho is the regression guard
// for the self-echo landmine: the bot posts an announcement that literally
// contains "/ace leave", and Meet renders the bot's own message back in the chat
// panel. If the loop scanned it, the bot would leave itself within one poll. The
// own-message guard must suppress command parsing for that echoed line while
// still capturing it for the audit trail.
func TestInteractive_DoesNotSelfTriggerOnAnnouncementEcho(t *testing.T) {
	h := newTestInteractiveHandler()

	// The chat read echoes back the bot's own announcement (as Meet does), then
	// the call ends so the loop terminates on its own.
	textJSON, _ := json.Marshal(meetAnnouncementText)
	echo := `[{"index":0,"sender":"You","text":` + string(textJSON) + `}]`
	page := &fakeMeetPage{chatReads: []string{echo}}
	go func() {
		time.Sleep(15 * time.Millisecond)
		page.mu.Lock()
		page.ended = true
		page.mu.Unlock()
	}()

	botMessages := map[string]struct{}{normalizeChatText(meetAnnouncementText): {}}
	noSegments := func() ([]TranscriptSegment, error) { return nil, nil }

	out := h.waitForMeetingEndInteractive(JobContext{}, page, testParams(), noSegments, time.Millisecond, botMessages)

	if out.endReason == "ace_command_leave" {
		t.Fatal("bot self-triggered a leave from its own announcement echo")
	}
	if page.leaveHit {
		t.Error("leave click must not run from the bot's own announcement echo")
	}
	if len(out.recognized) != 0 {
		t.Errorf("no command should be recognized from the bot's own echo; got %+v", out.recognized)
	}
	// The echoed message is still captured for the audit trail.
	if len(out.chat) != 1 {
		t.Errorf("captured chat = %d, want 1 (own echo still captured for audit)", len(out.chat))
	}
}
