package pairingdisplay

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// ansiClearHome clears the screen and homes the cursor — the same idiom
// internal/ui/devicecode.go's enrollment code box uses, minus any TUI
// framework, since this writes directly to a VT character device.
const ansiClearHome = "\x1b[2J\x1b[H"

// renderShowFrame builds the console frame for a pending pairing code: clear
// + home, an explanatory banner (product decision, citadel #659: always show
// the banner, regardless of whether the console appears in active use — see
// the package doc), the requester line, the expiry, and — LAST — the code in
// large block digits.
//
// Ordering is load-bearing, not cosmetic (caught in review): requested_by is
// free text under a length bound but, unlike code, was not charset-validated
// upstream, so an embedded ANSI/control sequence (e.g. another
// "\x1b[2J\x1b[H") could erase an already-rendered code while Show still
// reports delivered:true — the exact false-positive §8's governing rule
// forbids, since it suppresses the backend's linked-device fallback with
// nothing actually on screen. Two independent defenses, kept together
// deliberately: SanitizeText below strips control bytes from requested_by
// regardless of caller, AND the code is rendered last so even a FUTURE
// free-text field added to this frame cannot clobber it.
func renderShowFrame(req ShowRequest) string {
	var b strings.Builder
	b.WriteString(ansiClearHome)
	b.WriteString(bannerBox())
	b.WriteString("\n")
	if rb := truncateTrim(SanitizeText(req.RequestedBy), pairingRequestedByRenderMaxLen); rb != "" {
		fmt.Fprintf(&b, "  Requested by: %s\n\n", rb)
	}
	fmt.Fprintf(&b, "  Valid until %s (%s)\n\n", req.ExpiresAt.UTC().Format("15:04 MST"), humanTTL(req.TTL))
	b.WriteString(bigDigits(req.Code))
	b.WriteString("\n")
	return b.String()
}

// renderClearFrame builds the frame written when a display is cleared
// (TTL expiry, CLEAR_PAIRING_CODE, shutdown, or startup reconcile). The
// getty prompt was overwritten at show time; the next keypress redraws it.
func renderClearFrame(note string) string {
	var b strings.Builder
	b.WriteString(ansiClearHome)
	fmt.Fprintf(&b, "  AceTeam %s.\n", note)
	return b.String()
}

func bannerBox() string {
	lines := []string{
		"AceTeam pairing prompt",
		"Someone is requesting access to this machine.",
	}
	width := 0
	for _, l := range lines {
		if len(l) > width {
			width = len(l)
		}
	}
	border := strings.Repeat("=", width+4)
	var b strings.Builder
	b.WriteString(border + "\n")
	for _, l := range lines {
		fmt.Fprintf(&b, "  %s\n", l)
	}
	b.WriteString(border + "\n\n")
	return b.String()
}

func humanTTL(ttl time.Duration) string {
	mins := int(ttl.Round(time.Minute) / time.Minute)
	if mins <= 0 {
		secs := int(ttl.Round(time.Second) / time.Second)
		if secs <= 1 {
			return "for the next second"
		}
		return fmt.Sprintf("for the next %d seconds", secs)
	}
	if mins == 1 {
		return "for the next minute"
	}
	return fmt.Sprintf("for the next %d minutes", mins)
}

// pairingRequestedByRenderMaxLen mirrors internal/worker's
// pairingRequestedByMaxLen (the two packages cannot share a const across
// the module boundary without an import cycle risk, so this is a second,
// deliberately-named copy — see truncateTrim's rune-safety note for why
// both layers truncate independently rather than trusting the caller).
const pairingRequestedByRenderMaxLen = 80

// truncateTrim trims s and bounds it to at most max BYTES without splitting
// a multi-byte UTF-8 rune (backing off to the last full rune boundary at or
// before max, rather than an unconditional byte slice that could corrupt a
// trailing multi-byte character).
func truncateTrim(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	trimmed := s[:max]
	for len(trimmed) > 0 {
		r, size := utf8.DecodeLastRuneInString(trimmed)
		if r != utf8.RuneError || size != 1 {
			break
		}
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed
}

// SanitizeText strips ASCII control bytes (0x00-0x1F, including ESC 0x1B —
// the byte that begins every ANSI escape sequence — and DEL 0x7F) from s.
// It is applied to every free-text field rendered onto the console
// (currently just requested_by; code is instead REJECTED outright on any
// non-printable-ASCII byte — see internal/worker/pairing_display.go's
// isPrintableASCII — because a caller-supplied code has no legitimate
// reason to contain anything else, whereas requested_by is genuinely free
// text that may include non-ASCII UTF-8, e.g. an accented name). Multi-byte
// UTF-8 continuation/lead bytes are all >=0x80 and are therefore untouched
// by this filter — it only ever removes single-byte ASCII control
// characters, so it cannot corrupt a valid UTF-8 sequence.
//
// Exported so both the render path here (the last line of defense — see
// renderShowFrame's doc comment) and internal/worker/pairing_display.go's
// payload parsing (the first line of defense, applied where requested_by
// is parsed/bounded) use the SAME implementation rather than two that could
// drift apart.
func SanitizeText(s string) string {
	if !strings.ContainsFunc(s, isControlByteRune) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7F {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func isControlByteRune(r rune) bool {
	return (r >= 0 && r < 0x20) || r == 0x7F
}

// bigDigits renders code in a 3-row block font. Digits 0-9 get a proper
// 7-segment glyph (today's backend sends 8-digit codes); any other
// printable-ASCII character falls back to a plain single-character cell —
// the code is opaque per the payload contract (internal/worker/pairing_display.go)
// and citadel does not assume it is numeric.
func bigDigits(code string) string {
	var rows [3]strings.Builder
	for _, r := range code {
		glyph := glyphFor(r)
		for i := 0; i < 3; i++ {
			rows[i].WriteString(glyph[i])
			rows[i].WriteString(" ")
		}
	}
	var b strings.Builder
	for i := 0; i < 3; i++ {
		b.WriteString("  ")
		b.WriteString(rows[i].String())
		b.WriteString("\n")
	}
	return b.String()
}

var digitGlyphs = map[rune][3]string{
	'0': {" _ ", "| |", "|_|"},
	'1': {"   ", "  |", "  |"},
	'2': {" _ ", " _|", "|_ "},
	'3': {" _ ", " _|", " _|"},
	'4': {"   ", "|_|", "  |"},
	'5': {" _ ", "|_ ", " _|"},
	'6': {" _ ", "|_ ", "|_|"},
	'7': {" _ ", "  |", "  |"},
	'8': {" _ ", "|_|", "|_|"},
	'9': {" _ ", "|_|", " _|"},
}

// glyphFor returns the 3-row cell for one code character. Defense in depth
// beyond internal/worker's isPrintableASCII gate (which already REJECTS a
// code containing a non-printable-ASCII byte before it ever reaches this
// package): any rune outside the printable-ASCII range (0x20-0x7E) is
// substituted with '?' rather than embedded verbatim, so a future/alternate
// caller of Manager.Show that skips that validation still cannot get a raw
// control byte written into the console output via the code field.
func glyphFor(r rune) [3]string {
	if g, ok := digitGlyphs[r]; ok {
		return g
	}
	if r < 0x20 || r > 0x7E {
		r = '?'
	}
	return [3]string{"   ", " " + string(r) + " ", "   "}
}
