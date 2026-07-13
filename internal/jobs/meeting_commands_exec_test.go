package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newTestExecutor builds a meetingCommandExecutor with a capturing chat poster, a
// fixed clock (elapsed = 125s), an empty transcript, and an injected summarizer.
// posted collects every chat line the executor emits.
func newTestExecutor(summarize meetingSummarizer) (*meetingCommandExecutor, *[]string, *interactiveOutcome, *meetingTranscriptBuffer) {
	var posted []string
	out := &interactiveOutcome{}
	buf := &meetingTranscriptBuffer{}
	start := time.Unix(1_700_000_000, 0)
	e := &meetingCommandExecutor{
		log:        func(string, string, ...any) {},
		postChat:   func(text string) { posted = append(posted, text) },
		startTime:  start,
		now:        func() time.Time { return start.Add(125 * time.Second) },
		transcript: buf,
		summarize:  summarize,
		out:        out,
	}
	return e, &posted, out, buf
}

func dispatchWord(e *meetingCommandExecutor, kind CommandKind, word, instruction string) {
	e.dispatch(RecognizedCommand{Kind: kind, Command: word, Instruction: instruction})
}

func TestExecutor_Help(t *testing.T) {
	e, posted, _, _ := newTestExecutor(nil)
	dispatchWord(e, CommandHelp, "help", "")
	if len(*posted) != 1 || (*posted)[0] != meetingHelpText {
		t.Fatalf("help posted %v, want the help text", *posted)
	}
	// The help text must be a single line so Meet cannot split it into rows that
	// evade the self-echo seed (each row would re-parse the "/ace ..." tokens).
	if strings.ContainsAny(meetingHelpText, "\n\r") {
		t.Error("help text must be a single line")
	}
}

func TestExecutor_Status(t *testing.T) {
	e, posted, _, buf := newTestExecutor(nil)
	buf.add(TranscriptSegment{Start: 0, End: 2, Text: "hello"})
	buf.add(TranscriptSegment{Start: 2, End: 4, Text: "world"})
	dispatchWord(e, CommandStatus, "status", "")
	if len(*posted) != 1 {
		t.Fatalf("status posted %d messages, want 1", len(*posted))
	}
	got := (*posted)[0]
	for _, want := range []string{"recording", "02:05", "2 transcript segments"} {
		if !strings.Contains(got, want) {
			t.Errorf("status %q missing %q", got, want)
		}
	}
}

func TestExecutor_NoteWithText(t *testing.T) {
	e, posted, out, _ := newTestExecutor(nil)
	dispatchWord(e, CommandNote, "note", "buy more GPUs")
	if len(out.notes) != 1 {
		t.Fatalf("notes = %v, want one entry", out.notes)
	}
	if !strings.Contains(out.notes[0], "NOTE:") || !strings.Contains(out.notes[0], "buy more GPUs") || !strings.Contains(out.notes[0], "02:05") {
		t.Errorf("note entry %q missing label/timestamp/text", out.notes[0])
	}
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "noted") {
		t.Errorf("note confirmation = %v, want a 'noted' confirmation", *posted)
	}
}

func TestExecutor_NoteWithoutText(t *testing.T) {
	e, posted, out, _ := newTestExecutor(nil)
	dispatchWord(e, CommandNote, "note", "   ")
	if len(out.notes) != 0 {
		t.Errorf("empty note must not append an entry; got %v", out.notes)
	}
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "usage") {
		t.Errorf("empty note should post a usage hint; got %v", *posted)
	}
}

func TestExecutor_Action(t *testing.T) {
	e, posted, out, _ := newTestExecutor(nil)
	dispatchWord(e, CommandAction, "action", "Alice to send the deck")
	if len(out.notes) != 1 || !strings.Contains(out.notes[0], "ACTION:") || !strings.Contains(out.notes[0], "Alice to send the deck") {
		t.Fatalf("action entry = %v, want an ACTION entry", out.notes)
	}
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "action item") {
		t.Errorf("action confirmation = %v", *posted)
	}
}

func TestExecutor_SummaryEmptyBuffer(t *testing.T) {
	called := false
	e, posted, _, _ := newTestExecutor(func(context.Context, string) (string, error) {
		called = true
		return "should not run", nil
	})
	dispatchWord(e, CommandSummary, "summary", "")
	if called {
		t.Error("summarizer must NOT be called on an empty transcript")
	}
	if len(*posted) != 1 || !strings.Contains(strings.ToLower((*posted)[0]), "nothing to summarize") {
		t.Errorf("empty-buffer summary = %v, want a 'nothing to summarize' message", *posted)
	}
}

func TestExecutor_SummaryModelDown(t *testing.T) {
	e, posted, _, buf := newTestExecutor(func(context.Context, string) (string, error) {
		return "", errors.New("connection refused")
	})
	buf.add(TranscriptSegment{Start: 0, End: 3, Text: "we discussed the roadmap"})
	dispatchWord(e, CommandSummary, "summary", "")
	if len(*posted) != 1 || !strings.Contains(strings.ToLower((*posted)[0]), "unavailable") {
		t.Errorf("vLLM-down summary = %v, want a graceful fallback message", *posted)
	}
}

func TestExecutor_SummarySuccessIsSingleLine(t *testing.T) {
	e, posted, _, buf := newTestExecutor(func(_ context.Context, transcript string) (string, error) {
		if !strings.Contains(transcript, "roadmap") {
			t.Errorf("summarizer got transcript %q, want it to include the buffer text", transcript)
		}
		return "Line one.\nLine two.\n\nLine three.", nil
	})
	buf.add(TranscriptSegment{Start: 0, End: 3, Text: "we discussed the roadmap"})
	dispatchWord(e, CommandSummary, "summary", "")
	if len(*posted) != 1 {
		t.Fatalf("summary posted %d, want 1", len(*posted))
	}
	got := (*posted)[0]
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("summary post %q must be a single line (self-echo safety)", got)
	}
	if !strings.Contains(got, "Line one. Line two. Line three.") {
		t.Errorf("summary %q did not collapse newlines to spaces", got)
	}
}

func TestExecutor_GenericKindIsNoOp(t *testing.T) {
	e, posted, out, _ := newTestExecutor(nil)
	// A captured-but-not-executed kind must produce no chat and no notes.
	e.dispatch(RecognizedCommand{Kind: CommandGeneric, Command: "frobnicate"})
	if len(*posted) != 0 || len(out.notes) != 0 {
		t.Errorf("generic dispatch had side effects: posted=%v notes=%v", *posted, out.notes)
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		0:                  "00:00",
		65 * time.Second:   "01:05",
		125 * time.Second:  "02:05",
		3661 * time.Second: "1:01:01",
		-5 * time.Second:   "00:00",
	}
	for d, want := range cases {
		if got := formatElapsed(d); got != want {
			t.Errorf("formatElapsed(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestParseCommand_NewVerbs(t *testing.T) {
	cases := []struct {
		line  string
		kind  CommandKind
		cmd   string
		instr string
	}{
		{"/ace help", CommandHelp, "help", ""},
		{"/ace status", CommandStatus, "status", ""},
		{"/ace note buy more GPUs", CommandNote, "note", "buy more GPUs"},
		{"/ace action Alice to send the deck", CommandAction, "action", "Alice to send the deck"},
		{"/ace summary", CommandSummary, "summary", ""},
	}
	for _, c := range cases {
		got, ok := ParseCommand(c.line, SourceChat)
		if !ok {
			t.Errorf("ParseCommand(%q) not recognized", c.line)
			continue
		}
		if got.Kind != c.kind || got.Command != c.cmd || got.Instruction != c.instr {
			t.Errorf("ParseCommand(%q) = {kind:%q cmd:%q instr:%q}, want {kind:%q cmd:%q instr:%q}",
				c.line, got.Kind, got.Command, got.Instruction, c.kind, c.cmd, c.instr)
		}
	}
}
