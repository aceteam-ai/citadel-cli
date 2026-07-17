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
	"os"
	"path/filepath"
	"time"
)

// Fallback streaming cadence/margin when the handler's values are unset (zero).
// The worker populates the handler from the persisted meeting config (which
// clamps its own defaults), so these only apply to a hand-constructed handler.
const (
	defaultStreamingInterval = 15 * time.Second
	defaultStreamingWindow   = 10 * time.Second
	// defaultStreamingMaxWindow caps the trailing audio each rolling pass
	// re-transcribes (see meeting_transcribe_window.go). Far larger than
	// defaultStreamingWindow + defaultStreamingInterval so a segment always
	// stabilizes and is emitted before it slides out of the re-fed window.
	defaultStreamingMaxWindow = 90 * time.Second
	// rollingShutdownGrace bounds how long the interactive loop waits for the
	// rolling-transcription goroutine to finish after signalling stop. Passes are
	// windowed (bounded per pass), so this is generous headroom; it exists only so
	// a pathologically slow final pass can never wedge the terminal result. If it
	// trips, transcribeMu still serializes the end-of-call batch transcribe.
	rollingShutdownGrace = 30 * time.Second
)

// stopRolling signals the rolling-transcription goroutine to stop and waits
// (bounded) for it to finish, so the caller returns with the goroutine already
// stopped. Deferred on every interactive-loop exit path. Bounded because the
// caller's next step (the end-of-call batch transcribe) must always run — a stuck
// final pass is contained by transcribeMu, not by blocking Execute forever here.
func (h *MeetingJoinHandler) stopRolling(ctx JobContext, stop chan struct{}, done <-chan struct{}) {
	close(stop)
	select {
	case <-done:
	case <-time.After(rollingShutdownGrace):
		ctx.Log("warn", "     - rolling transcriber did not stop within %s; proceeding to batch transcribe (transcribeMu still serializes)", rollingShutdownGrace)
	}
}

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
	// notes are timestamped NOTE/ACTION entries appended by the in-call `/ace
	// note` and `/ace action` commands, surfaced in the MEETING_JOIN result.
	notes []string
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
	rollingDone := make(chan struct{})

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
		defer close(rollingDone)
		defer func() {
			if r := recover(); r != nil {
				ctx.Log("warn", "     - rolling transcription goroutine panicked (recovered, recording unaffected): %v", r)
			}
		}()
		rt.Run(stop)
	}()

	// On EVERY exit path (leave, call-ended, or duration cap) signal the rolling
	// transcriber to stop and WAIT (bounded) for it to actually finish before
	// returning. This is the Bug B lifecycle fix: Execute must then proceed to the
	// end-of-call batch transcribe with the rolling goroutine already stopped, so
	// no pass keeps "firing after leaving" and the terminal result is always
	// produced (found live on node 1084, 2026-07-16). The wait is bounded so a
	// pathologically slow final pass can never wedge the terminal outcome —
	// transcribeMu still serializes the batch transcribe for correctness if the
	// grace trips.
	defer h.stopRolling(ctx, stop, rollingDone)

	out := interactiveOutcome{endReason: "max_duration_reached"}
	seenChat := make(map[int]struct{})
	var leaveRequested bool

	// transcript accumulates stabilized segments so `/ace status` and `/ace
	// summary` can read the live conversation. Written only in drainSegments.
	transcript := &meetingTranscriptBuffer{}

	// exec runs the non-destructive `/ace` verbs (help/status/note/action/
	// summary). postChat SEEDS botMessages before every post so the loop never
	// re-parses the bot's own output (the #1 self-echo bug). startTime is the loop
	// entry, i.e. the moment recording is under way.
	exec := &meetingCommandExecutor{
		log: ctx.Log,
		postChat: func(text string) {
			botMessages[normalizeChatText(text)] = struct{}{}
			if err := postMeetChat(page, text); err != nil {
				ctx.Log("warn", "     - could not post `/ace` reply to chat (non-fatal, selector may be stale): %v", err)
			}
		},
		startTime:  time.Now(),
		now:        time.Now,
		transcript: transcript,
		summarize:  localVLLMSummarize,
		out:        &out,
	}

	// handleLine parses one transcript/chat line for an invocation and records
	// it. `/ace leave` is auto-executed by the loop below (it must break the
	// loop); the other registered verbs are executed here (non-destructive: they
	// only read state, append to a buffer, or post chat). Generic commands and
	// wake-phrase instructions are captured/surfaced for the agent layer.
	handleLine := func(line string, source CommandSource) {
		cmd, ok := ParseCommand(line, source)
		if !ok {
			return
		}
		ctx.Log("info", "     - recognized in-call command (source=%s kind=%s cmd=%q)", source, cmd.Kind, cmd.Command)
		out.recognized = append(out.recognized, cmd)
		if cmd.Kind == CommandLeave {
			leaveRequested = true
			return
		}
		exec.dispatch(cmd)
	}

	deadline := time.Now().Add(p.maxDuration())
	for time.Now().Before(deadline) {
		// 1. Drain newly-stabilized transcript segments (non-blocking).
		h.drainSegments(segCh, &out, transcript, handleLine)

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
// blocking, updating the outcome's streamed count, accumulating each into the
// live transcript buffer (for `/ace status`/`/ace summary`), and feeding each to
// handleLine. Extracted so the non-blocking drain is readable and reusable.
func (h *MeetingJoinHandler) drainSegments(segCh <-chan TranscriptSegment, out *interactiveOutcome, transcript *meetingTranscriptBuffer, handleLine func(string, CommandSource)) {
	for {
		select {
		case s := <-segCh:
			out.streamedSegments++
			transcript.add(s)
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

// transcribeSegments transcribes the recent TAIL of the growing recording and
// returns its segments (on the ABSOLUTE recording timeline) for one rolling pass.
// It serializes sidecar access via h.transcribeMu so a rolling pass never
// overlaps the final batch transcribe (or another pass) — they would otherwise
// hit the whisper sidecar and read the in-progress WAV concurrently. An error is
// returned (not fatal) so the rolling driver logs and retries on the next tick.
//
// WINDOWING (the latency fix): the whisper sidecar transcribes a whole file with
// no offset/streaming API, so re-transcribing the entire wav-so-far every pass
// makes each pass slower as the call grows. Instead this clips only the trailing
// h.streamingMaxWindow() of audio into a workspace-local scratch WAV
// (meeting_transcribe_window.go) and transcribes THAT, then shifts the returned
// segment times by the clip's start offset back onto the absolute timeline so the
// rolling driver's absolute-start dedup/stabilization contract is preserved. The
// tradeoff — segments older than the window are never re-fed — is safe because
// the window (default 90s) is far larger than the stability margin + cadence, so
// a segment always stabilizes and is emitted before it slides out of the window.
// If clipping fails (e.g. the header is not fully written yet early in a call),
// it falls back to whole-file transcription: correctness over latency.
//
// Known quality wrinkle (live-verification item): each clip's LEADING edge
// starts mid-utterance ~window seconds back, so whisper may occasionally emit a
// short garbled fragment there. Absolute-start bucket dedup drops the common case
// (that audio was already emitted with full context in a prior pass), but a
// fragment landing in a never-before-started bucket can slip into the streamed
// buffer. This is cosmetic — the STORED transcript is the untouched end-of-call
// batch pass, and garbled text won't match an `/ace` token.
func (h *MeetingJoinHandler) transcribeSegments(ctx JobContext, meetingID, wavPath string) ([]TranscriptSegment, error) {
	h.transcribeMu.Lock()
	defer h.transcribeMu.Unlock()

	transcribePath := wavPath
	var offset float64
	clipPath := meetingWindowWavPath(h.WorkspaceDir, meetingID)
	// Create the node-owned scratch dir (meetingScratchDirName) up front: it is
	// distinct from the container-owned meetings/ dir so clipping never depends on
	// write access there. MkdirAll on a dir this handler owns is cheap and
	// idempotent; a failure here just degrades this pass to whole-file transcribe.
	if err := os.MkdirAll(filepath.Dir(clipPath), 0o700); err != nil {
		ctx.Log("warn", "     - could not create node-owned rolling scratch dir, falling back to whole-file transcribe (non-fatal): %v", err)
	} else if off, err := clipWavTail(wavPath, clipPath, h.streamingMaxWindow()); err != nil {
		ctx.Log("warn", "     - rolling window clip failed, falling back to whole-file transcribe (non-fatal): %v", err)
	} else {
		transcribePath = clipPath
		offset = off
	}

	synthetic := syntheticTranscribeJob(meetingID+"-rolling", transcribePath)
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
	if offset > 0 {
		for i := range decoded.Segments {
			decoded.Segments[i].Start += offset
			decoded.Segments[i].End += offset
		}
	}
	return decoded.Segments, nil
}

// streamingMaxWindow resolves the trailing-audio cap for a rolling pass, falling
// back to the package default when unset.
func (h *MeetingJoinHandler) streamingMaxWindow() time.Duration {
	if h.StreamingMaxWindow > 0 {
		return h.StreamingMaxWindow
	}
	return defaultStreamingMaxWindow
}
