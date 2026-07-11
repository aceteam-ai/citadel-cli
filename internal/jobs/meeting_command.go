// internal/jobs/meeting_command.go
//
// In-call command monitor (issue #5435, epic #5097 — turning the passive Meet
// notetaker into a controllable in-call agent). This file is a PURE, fully
// unit-tested parser: it scans a single line of text (a transcript segment OR a
// captured Meet chat line) for an invocation and returns a structured
// RecognizedCommand, with NO side effects and NO DOM/browser knowledge. The
// wiring that acts on the result lives in meeting_join.go; the recognition
// logic lives here so it can be tested exhaustively without a browser.
//
// SAFETY PROPERTY (deliberate): the ONLY invocation this node auto-executes is a
// literal `/ace leave`, which ends the bot's participation. Everything else —
// a generic `/ace <command>` or a wake-phrase instruction — is CAPTURED and
// surfaced (result + logs) for the cloud/agent layer to interpret; this node
// never executes free-form instructions. Requiring the literal `/ace ` token
// (not a fuzzy homophone) for the one destructive action means ambient speech
// like "does the ace leave early?" can never end a meeting.
package jobs

import (
	"strings"
	"unicode"
)

// aceCommandToken is the literal invocation prefix for typed/spoken commands:
// `/ace <command>`. Matched case-insensitively but NOT fuzzily — a real slash is
// required, so ordinary speech transcribed without a slash cannot trigger a
// command (see the SAFETY PROPERTY above).
const aceCommandToken = "/ace"

// aceWakePhrases are the spoken wake phrases that introduce a free-form
// instruction: "hey aceteam, <instructions>". Multiple spellings are accepted
// because whisper renders the brand inconsistently ("aceteam" / "ace team").
// Matched case-insensitively as a substring so surrounding speech in the same
// segment ("... so hey aceteam, summarize that") still matches.
var aceWakePhrases = []string{"hey aceteam", "hey ace team"}

// aceSpokenCommandPrefixes are best-guess spoken renderings of the `/ace` token
// for when a participant SAYS the command out loud and whisper transcribes the
// slash as a word ("slash ace leave"). This is a HEURISTIC, not a guarantee —
// it is deliberately kept separate from the literal aceCommandToken path and is
// only consulted for command recognition, never promoted to the auto-executed
// leave path unless the parsed command is itself an explicit registry command.
//
// LIVE-TUNING REQUIRED (best-guess, unverified) — 2026-07-11: whether whisper
// renders a spoken "/ace" as "slash ace", "slash-ace", or something else can
// only be confirmed by a human speaking commands on a live call. Keep these
// together for one-place tuning.
var aceSpokenCommandPrefixes = []string{"slash ace", "slash-ace"}

// CommandKind classifies a recognized invocation.
type CommandKind string

const (
	// CommandLeave is the one auto-executed action: end the bot's participation.
	CommandLeave CommandKind = "leave"
	// CommandGeneric is a `/ace <command>` whose command word is not in the
	// registry. It is captured and surfaced for the agent layer, not executed.
	CommandGeneric CommandKind = "generic"
	// CommandInstruction is a wake-phrase free-form instruction
	// ("hey aceteam, <instructions>"). Captured and surfaced, not executed.
	CommandInstruction CommandKind = "instruction"
)

// CommandSource records where a recognized command was observed, so a caller
// (and the audit trail) can distinguish a spoken command from a typed one.
type CommandSource string

const (
	// SourceTranscript means the command came from a rolling transcript segment.
	SourceTranscript CommandSource = "transcript"
	// SourceChat means the command came from a Meet in-call chat line.
	SourceChat CommandSource = "chat"
)

// RecognizedCommand is the structured result of parsing one line. Command is the
// registry keyword for CommandLeave/CommandGeneric (lower-cased, no arguments);
// Instruction carries the free-form remainder for CommandGeneric (the args after
// the command word) and CommandInstruction (everything after the wake phrase).
// Raw is the original untrimmed line, preserved for the audit trail.
type RecognizedCommand struct {
	Kind        CommandKind   `json:"kind"`
	Command     string        `json:"command,omitempty"`
	Instruction string        `json:"instruction,omitempty"`
	Source      CommandSource `json:"source"`
	Raw         string        `json:"raw"`
}

// commandRegistry maps a `/ace <word>` command keyword to its kind. It is the
// small, extensible surface for adding node-executed commands: today only
// "leave" is auto-executed; a new entry here (plus wiring in meeting_join.go)
// adds another. A word absent from the registry parses as CommandGeneric.
var commandRegistry = map[string]CommandKind{
	"leave": CommandLeave,
}

// ParseCommand scans one line of text (a transcript segment or a chat line) for
// an in-call invocation and returns a RecognizedCommand plus ok=true when one is
// found. It is pure and allocation-light; a non-match returns (zero, false)
// rather than an error because "no command in this line" is the overwhelmingly
// common case, not an exceptional one.
//
// Precedence: the literal `/ace` token wins over the spoken-command heuristic,
// which wins over the wake phrase. Within a line the FIRST matching invocation
// is used (pick-first) so a line mentioning several never ambiguously executes.
func ParseCommand(line string, source CommandSource) (RecognizedCommand, bool) {
	lower := strings.ToLower(line)

	// 1. Literal `/ace <command>` — the authoritative path (only source of the
	//    auto-executed leave). The token must sit on a boundary: not glued to a
	//    preceding word (so "a/ace@host" is not a command) and followed by a
	//    separator (so "/aceleave" is not a command).
	if rest, ok := tokenRestWithBoundary(line, lower, aceCommandToken); ok {
		if cmd, ok := parseAceCommandRest(rest, source, line); ok {
			return cmd, true
		}
	}

	// 2. Spoken-command heuristic ("slash ace leave"). Flagged, best-effort.
	for _, prefix := range aceSpokenCommandPrefixes {
		if rest, ok := tokenRestWithBoundary(line, lower, prefix); ok {
			if cmd, ok := parseAceCommandRest(rest, source, line); ok {
				return cmd, true
			}
		}
	}

	// 3. Wake phrase ("hey aceteam, <instructions>") — free-form capture only.
	for _, phrase := range aceWakePhrases {
		if idx := strings.Index(lower, phrase); idx >= 0 {
			rest := strings.TrimSpace(trimLeadingPunct(line[idx+len(phrase):]))
			if rest == "" {
				// A bare wake phrase with no instruction is not actionable.
				continue
			}
			return RecognizedCommand{
				Kind:        CommandInstruction,
				Instruction: rest,
				Source:      source,
				Raw:         line,
			}, true
		}
	}

	return RecognizedCommand{}, false
}

// parseAceCommandRest interprets the text after an `/ace` (or spoken-equivalent)
// token: the first word is the command keyword (registry lookup decides leave vs
// generic), and any remainder is captured as the instruction/arguments. Returns
// ok=false when there is no command word at all (a bare `/ace`), so callers can
// fall through to other invocation forms.
func parseAceCommandRest(rest string, source CommandSource, raw string) (RecognizedCommand, bool) {
	rest = trimLeadingPunct(rest)
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return RecognizedCommand{}, false
	}
	word := strings.ToLower(trimSurroundingPunct(fields[0]))
	if word == "" {
		return RecognizedCommand{}, false
	}
	instruction := strings.TrimSpace(rest[strings.Index(rest, fields[0])+len(fields[0]):])
	instruction = strings.TrimSpace(trimLeadingPunct(instruction))

	kind, known := commandRegistry[word]
	if !known {
		kind = CommandGeneric
	}
	return RecognizedCommand{
		Kind:        kind,
		Command:     word,
		Instruction: instruction,
		Source:      source,
		Raw:         raw,
	}, true
}

// tokenRestWithBoundary finds the FIRST occurrence of an ASCII token in lower
// (the lower-cased line) that sits on a word boundary, and returns the slice of
// the ORIGINAL line following it. A valid boundary means: the char before the
// token is the start of the line or a non-alphanumeric rune (so the token is not
// the tail of a larger word like "a/ace"), and the char after the token is the
// end of the line or a recognized separator (so "/aceleave" or "/ace@x" do not
// match while "/ace leave" and "/ace: leave" do). token must be ASCII; both
// invocation tokens are. Returns ok=false when no bounded occurrence exists.
func tokenRestWithBoundary(line, lower, token string) (string, bool) {
	search := 0
	for {
		rel := strings.Index(lower[search:], token)
		if rel < 0 {
			return "", false
		}
		idx := search + rel
		after := idx + len(token)
		beforeOK := idx == 0 || !isWordRune(rune(lower[idx-1]))
		afterOK := after >= len(lower) || isCommandSeparator(rune(lower[after]))
		if beforeOK && afterOK {
			return line[after:], true
		}
		search = idx + len(token)
	}
}

// isWordRune reports whether r is an alphanumeric character, i.e. part of a word.
// Used to reject a token glued to a preceding word.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// isCommandSeparator reports whether r is whitespace or a punctuation separator
// that may legitimately follow an invocation token ("/ace: leave", "/ace,leave").
// Notably '@' and alphanumerics are NOT separators, so a token buried inside an
// email or a longer word is not treated as a command.
func isCommandSeparator(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	return strings.ContainsRune(",:;.-—–", r)
}

// trimLeadingPunct strips leading whitespace and common separator punctuation
// (comma, colon, dash, period) that whisper or a typist may place right after
// the token, e.g. "/ace: leave" or "hey aceteam, ...".
func trimLeadingPunct(s string) string {
	return strings.TrimLeft(s, " \t\r\n,:;.-—–")
}

// trimSurroundingPunct strips separator/terminator punctuation from both ends of
// a single word so "leave." or "leave," matches the registry key "leave".
func trimSurroundingPunct(s string) string {
	return strings.Trim(s, " \t\r\n,.;:!?—–-\"'")
}
