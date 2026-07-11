// internal/jobs/meeting_interactive.go
//
// In-call interactive session coordinator (issue #5435, epic #5097). This wires
// the three additive capabilities — rolling transcription
// (meeting_transcribe_rolling.go), the `/ace` command monitor
// (meeting_command.go), and Meet chat capture (meeting_chat.go) — into the
// during-call poll loop, plus the self-announcement on admittance. It is the
// interactive replacement for meeting_join.go's plain waitForMeetingEnd, run
// only when StreamingEnabled; the batch record→transcribe path is untouched.
//
// HARD CONSTRAINT (the reason for every guard here): the batch pipeline already
// works in production. The interactive layer is STRICTLY ADDITIVE and must never
// crash or stall the recording. Concretely:
//   - Every DOM/CDP interaction (chat read, announcement, leave click, end
//     heuristics) is best-effort: an error is logged and the loop continues.
//   - Rolling transcription runs on its own goroutine wrapped in recover(), so a
//     panic in whisper/decoding cannot take down the recording.
//   - Sidecar access from rolling passes and the final batch transcribe is
//     serialized (h.transcribeMu) so they never hit the whisper sidecar — or
//     read the growing WAV — concurrently.
//   - The ONLY auto-executed action is a parsed literal `/ace leave` (see the
//     safety property in meeting_command.go).
package jobs

import (
	"encoding/json"
	"fmt"
	"time"
)

// Fallback streaming cadence/margin when the handler's values are unset (zero).
// The worker populates the handler from the persisted meeting config (which
// clamps its own defaults), so these only apply to a hand-constructed handler.
const (
	defaultStreamingInterval = 15 * time.Second
	defaultStreamingWindow   = 10 * time.Second
)

// meetPage is the browser surface the interactive session drives via page JS:
// chat read/post, end heuristics, and the leave click. *platform.MeetingBrowser
// satisfies it; a fake satisfies it in tests. Structurally identical to
// chatEvaluator; named separately for the wider interactive use.
type meetPage interface {
	Evaluate(expression string) (any, error)
}

// interactiveOutcome is what the interactive session observed across the call:
// why it ended, the captured chat, how many transcript segments streamed in, and
// which in-call commands were recognized (leave plus captured/surfaced generics
// and wake-phrase instructions). It feeds the additive fields of the MEETING_JOIN
// result.
type interactiveOutcome struct {
	endReason        string
	chat             []MeetChatMessage
	streamedSegments int
	recognized       []RecognizedCommand
}

func (h *MeetingJoinHandler) streamingInterval() time.Duration {
	if h.StreamingInterval > 0 {
		return h.StreamingInterval
	}
	return defaultStreamingInterval
}

func (h *MeetingJoinHandler) streamingWindow() time.Duration {
	if h.StreamingWindow > 0 {
		return h.StreamingWindow
	}
	return defaultStreamingWindow
}

// announceOnAdmission opens the chat panel and posts the consent/intro
// announcement (capability 4). Best-effort at every step: a stale chat selector
// logs and returns without disturbing the recording. Posting is attempted even
// if opening errored, since the panel may already be open.
//
// It ALWAYS records the announcement's normalized text into botMessages —
// regardless of whether the post succeeded — so the poll loop never scans the
// bot's OWN echoed announcement for commands. This matters because the
// announcement literally contains "/ace leave": without this guard, the bot
// would read its own message back and immediately leave the call. Seeding
// unconditionally means the guard is in place BEFORE the chat-post selectors are
// live-tuned to actually work (when the echo first appears).
func (h *MeetingJoinHandler) announceOnAdmission(ctx JobContext, page meetPage, botMessages map[string]struct{}) {
	botMessages[normalizeChatText(meetAnnouncementText)] = struct{}{}

	if err := openMeetChat(page); err != nil {
		ctx.Log("warn", "     - could not open Meet chat to announce (non-fatal, selector may be stale): %v", err)
	}
	if err := postMeetChat(page, meetAnnouncementText); err != nil {
		ctx.Log("warn", "     - could not post self-announcement to chat (non-fatal, selector may be stale): %v", err)
		return
	}
	ctx.Log("info", "     - posted self-announcement to Meet chat")
}

// waitForMeetingEndInteractive runs the during-call interactive loop until the
// call ends, an `/ace leave` is recognized, or the hard duration cap trips. It
// never errors: any interactive failure degrades to the batch behavior (a valid
// recording still gets transcribed at the end). transcribe is injected (built
// from h.transcribeSegments in production, a fake in tests). pollInterval is the
// loop cadence for chat/end/leave checks; the rolling transcriber ticks
// independently at h.streamingInterval().
func (h *MeetingJoinHandler) waitForMeetingEndInteractive(
	ctx JobContext,
	page meetPage,
	p meetingJoinParams,
	transcribe TranscribeFunc,
	pollInterval time.Duration,
	botMessages map[string]struct{},
) interactiveOutcome {
	segCh := make(chan TranscriptSegment, 64)
	stop := make(chan struct{})
	defer close(stop)

	// Rolling transcription on its own goroutine. recover() is load-bearing: a
	// panic in the injected transcribe (or decoding) must not crash the process
	// and lose the recording. Emit races the loop's stop via a select so it never
	// blocks after the loop has moved on.
	rt := &RollingTranscriber{
		Interval:   h.streamingInterval(),
		Window:     h.streamingWindow(),
		Transcribe: transcribe,
		Emit: func(s TranscriptSegment) {
			select {
			case segCh <- s:
			case <-stop:
			}
		},
		Log: func(level, msg string) { ctx.Log(level, "%s", msg) },
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ctx.Log("warn", "     - rolling transcription goroutine panicked (recovered, recording unaffected): %v", r)
			}
		}()
		rt.Run(stop)
	}()

	out := interactiveOutcome{endReason: "max_duration_reached"}
	seenChat := make(map[int]struct{})
	var leaveRequested bool

	// handleLine parses one transcript/chat line for an invocation and records
	// it. Only a literal `/ace leave` is auto-executed (below); generic commands
	// and wake-phrase instructions are captured/surfaced for the agent layer.
	handleLine := func(line string, source CommandSource) {
		cmd, ok := ParseCommand(line, source)
		if !ok {
			return
		}
		ctx.Log("info", "     - recognized in-call command (source=%s kind=%s cmd=%q)", source, cmd.Kind, cmd.Command)
		out.recognized = append(out.recognized, cmd)
		if cmd.Kind == CommandLeave {
			leaveRequested = true
		}
	}

	deadline := time.Now().Add(p.maxDuration())
	for time.Now().Before(deadline) {
		// 1. Drain newly-stabilized transcript segments (non-blocking).
		h.drainSegments(segCh, &out, handleLine)

		// 2. Read chat; feed new lines (dedup by append-only index).
		if msgs, err := readMeetChat(page); err != nil {
			ctx.Log("warn", "     - chat read failed (non-fatal, selector may be stale): %v", err)
		} else {
			for _, m := range msgs {
				if _, seen := seenChat[m.Index]; seen {
					continue
				}
				seenChat[m.Index] = struct{}{}
				out.chat = append(out.chat, m)
				// Never scan the bot's OWN echoed messages for commands (the
				// announcement contains "/ace leave"). Still captured above for the
				// audit trail; just not fed to the command monitor.
				if _, own := botMessages[normalizeChatText(m.Text)]; own {
					continue
				}
				handleLine(m.Line(), SourceChat)
			}
		}

		// 3. Act on a recognized /ace leave (the only auto-executed command).
		if leaveRequested {
			ctx.Log("info", "     - /ace leave recognized; leaving the call gracefully")
			if _, err := page.Evaluate(meetLeaveCallJS); err != nil {
				ctx.Log("warn", "     - graceful leave click errored (non-fatal, teardown closes the browser): %v", err)
			}
			out.endReason = "ace_command_leave"
			return out
		}

		// 4. Existing end heuristics (call ended / bot alone).
		if reason, ended := checkMeetingEnded(page); ended {
			out.endReason = reason
			return out
		}

		time.Sleep(pollInterval)
	}
	ctx.Log("info", "     - max meeting duration (%s) reached; leaving", p.maxDuration())
	return out
}

// drainSegments pulls all currently-buffered transcript segments without
// blocking, updating the outcome's streamed count and feeding each to
// handleLine. Extracted so the non-blocking drain is readable and reusable.
func (h *MeetingJoinHandler) drainSegments(segCh <-chan TranscriptSegment, out *interactiveOutcome, handleLine func(string, CommandSource)) {
	for {
		select {
		case s := <-segCh:
			out.streamedSegments++
			handleLine(s.Text, SourceTranscript)
		default:
			return
		}
	}
}

// checkMeetingEnded runs the two shared end heuristics: the "you left / call
// ended" text scan and the bot-alone participant-count signal. Both are
// best-guess (see meeting_join.go's LIVE-TUNING block); a read error is treated
// as "not ended" so a transient DOM glitch never ends the call early. Shared by
// the plain and interactive wait loops.
func checkMeetingEnded(page meetPage) (string, bool) {
	if v, err := page.Evaluate(meetIsEndedJS); err == nil {
		if b, ok := v.(bool); ok && b {
			return "call_ended", true
		}
	}
	if v, err := page.Evaluate(meetParticipantCountJS); err == nil {
		if n, ok := toInt(v); ok && n >= 0 && n <= 1 {
			return "alone_in_call", true
		}
	}
	return "", false
}

// transcribeSegments re-transcribes the growing recording and returns its
// segments, for one rolling pass. It serializes sidecar access via
// h.transcribeMu so a rolling pass never overlaps the final batch transcribe (or
// another pass) — they would otherwise hit the whisper sidecar and read the
// in-progress WAV concurrently. An error is returned (not fatal) so the rolling
// driver logs and retries on the next tick.
func (h *MeetingJoinHandler) transcribeSegments(ctx JobContext, meetingID, wavPath string) ([]TranscriptSegment, error) {
	h.transcribeMu.Lock()
	defer h.transcribeMu.Unlock()

	synthetic := syntheticTranscribeJob(meetingID+"-rolling", wavPath)
	raw, err := h.transcriber.Execute(ctx, synthetic)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Segments []TranscriptSegment `json:"segments"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode rolling transcript segments: %w", err)
	}
	return decoded.Segments, nil
}
