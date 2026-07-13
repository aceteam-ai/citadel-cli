// internal/jobs/meeting_commands_exec.go
//
// In-call `/ace` command execution (issue #5435, epic #5097). The parser
// (meeting_command.go) recognizes a command; this file ACTS on the
// non-destructive verbs — help, status, note, action, summary — while leave is
// handled inline by the interactive loop (it must break the loop, not just post
// chat). Execution is deliberately kept OUT of the parser and OUT of package
// globals: the executor is constructed with its exact dependencies (chat poster,
// recording start time, live transcript buffer, summarizer, clock) so it is
// unit-testable with fakes and never reaches into shared state.
//
// SELF-ECHO SAFETY (the #1 bug risk): every chat line the bot posts is seeded
// into botMessages BEFORE the post (unconditionally, like announceOnAdmission)
// so the interactive loop never re-parses the bot's own output as a command. All
// bot output is a SINGLE line — multi-line output could be split by Meet into
// separate chat rows, none of which would match the seeded full string, so a row
// carrying "/ace ..." would be parsed and re-executed in a loop.
package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// meetingTranscriptBuffer accumulates stabilized transcript segments during the
// call so `/ace status` can report how much has been captured and `/ace summary`
// can build a prompt. It is written and read only on the interactive loop's
// single goroutine, so it needs no locking.
type meetingTranscriptBuffer struct {
	segments []TranscriptSegment
}

// add appends one stabilized segment.
func (b *meetingTranscriptBuffer) add(s TranscriptSegment) {
	b.segments = append(b.segments, s)
}

// count returns how many segments have been captured (transcript health signal).
func (b *meetingTranscriptBuffer) count() int {
	return len(b.segments)
}

// text joins the captured segment texts into one whitespace-collapsed string for
// the summary prompt. Empty when nothing has streamed yet.
func (b *meetingTranscriptBuffer) text() string {
	parts := make([]string, 0, len(b.segments))
	for _, s := range b.segments {
		if t := strings.TrimSpace(s.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// meetingCommandExecutor holds everything the non-destructive `/ace` verbs need,
// injected rather than reached for. postChat seeds botMessages and posts to Meet
// chat; startTime + now yield elapsed duration; transcript is the live buffer;
// summarize is the (injectable) node-local vLLM call; out receives appended
// notes/action items for the MEETING_JOIN result.
type meetingCommandExecutor struct {
	log        func(level, format string, args ...any)
	postChat   func(text string)
	startTime  time.Time
	now        func() time.Time
	transcript *meetingTranscriptBuffer
	summarize  meetingSummarizer
	out        *interactiveOutcome
}

// dispatch executes one recognized non-destructive command. Unknown/destructive
// kinds are ignored here (leave is handled by the loop; generic/instruction are
// captured only), so dispatch is a no-op for anything it does not own.
func (e *meetingCommandExecutor) dispatch(cmd RecognizedCommand) {
	switch cmd.Kind {
	case CommandHelp:
		e.postChat(meetingHelpText)
	case CommandStatus:
		e.postChat(e.statusText())
	case CommandNote:
		e.appendEntry("NOTE", cmd.Instruction, "noted")
	case CommandAction:
		e.appendEntry("ACTION", cmd.Instruction, "action item recorded")
	case CommandSummary:
		e.postSummary()
	}
}

// meetingHelpText is the single-line command list posted for `/ace help`. Single
// line by design (see the SELF-ECHO SAFETY note); the seed suppresses re-parsing
// the "/ace ..." tokens it contains.
const meetingHelpText = "AceTeam commands: /ace help · /ace status · /ace note <text> · " +
	"/ace action <text> · /ace summary · /ace leave"

// statusText reports recording state, elapsed duration, and transcript health.
func (e *meetingCommandExecutor) statusText() string {
	return fmt.Sprintf("Status: recording · %s elapsed · %d transcript segments captured",
		formatElapsed(e.elapsed()), e.transcript.count())
}

// appendEntry records a timestamped NOTE/ACTION in the outcome buffer and
// confirms in chat. Empty text is not recorded — the bot posts a usage hint
// instead so a stray "/ace note" does not litter the buffer with blanks.
func (e *meetingCommandExecutor) appendEntry(label, text, confirmation string) {
	text = strings.TrimSpace(collapseToSingleLine(text))
	if text == "" {
		e.postChat(fmt.Sprintf("Nothing to record — usage: /ace %s <text>", strings.ToLower(label)))
		return
	}
	entry := fmt.Sprintf("[%s] %s: %s", formatElapsed(e.elapsed()), label, text)
	e.out.notes = append(e.out.notes, entry)
	e.postChat(fmt.Sprintf("%s: %s", confirmation, text))
}

// postSummary builds a prompt from the live transcript, calls the summarizer
// synchronously, and posts the result. It NEVER lets a summarizer failure crash
// the loop: an empty buffer or an unreachable model yields a clear fallback chat
// message. Runs on the loop goroutine so it shares the transcript buffer with no
// race; the summarizer's own tight timeout bounds the pause.
func (e *meetingCommandExecutor) postSummary() {
	transcript := e.transcript.text()
	if strings.TrimSpace(transcript) == "" {
		e.postChat("No transcript captured yet — nothing to summarize.")
		return
	}
	summary, err := e.summarize(context.Background(), transcript)
	if err != nil || strings.TrimSpace(summary) == "" {
		if err != nil {
			e.log("warn", "     - /ace summary: local model unavailable (non-fatal): %v", err)
		}
		e.postChat("Summary unavailable right now — the local model is unreachable.")
		return
	}
	e.postChat("Summary: " + collapseToSingleLine(summary))
}

// elapsed is the wall time since recording started.
func (e *meetingCommandExecutor) elapsed() time.Duration {
	return e.now().Sub(e.startTime)
}

// formatElapsed renders a duration as H:MM:SS (or MM:SS under an hour) for the
// meeting-relative timestamps on notes and the status line.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// collapseToSingleLine flattens any newlines/runs of whitespace in a string to
// single spaces so every bot chat post stays on ONE line (see the SELF-ECHO
// SAFETY note). Model summaries and pasted note text can arrive multi-line.
func collapseToSingleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
