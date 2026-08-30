package pairingdisplay

import (
	"fmt"
	"strings"
	"time"
)

// ansiClearHome clears the screen and homes the cursor — the same idiom
// internal/ui/devicecode.go's enrollment code box uses, minus any TUI
// framework, since this writes directly to a VT character device.
const ansiClearHome = "\x1b[2J\x1b[H"

// renderShowFrame builds the console frame for a pending pairing code: clear
// + home, an explanatory banner (product decision, citadel #659: always show
// the banner, regardless of whether the console appears in active use — see
// the package doc), the code in large block digits, the requester line, and
// both absolute and relative expiry so a stale render after a crash is
// self-describing (design doc §9.1.4 / §12).
func renderShowFrame(req ShowRequest) string {
	var b strings.Builder
	b.WriteString(ansiClearHome)
	b.WriteString(bannerBox())
	b.WriteString("\n")
	b.WriteString(bigDigits(req.Code))
	b.WriteString("\n")
	if rb := truncateTrim(req.RequestedBy, 80); rb != "" {
		fmt.Fprintf(&b, "  Requested by: %s\n\n", rb)
	}
	fmt.Fprintf(&b, "  Valid until %s (%s)\n", req.ExpiresAt.UTC().Format("15:04 MST"), humanTTL(req.TTL))
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

func truncateTrim(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
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

func glyphFor(r rune) [3]string {
	if g, ok := digitGlyphs[r]; ok {
		return g
	}
	return [3]string{"   ", " " + string(r) + " ", "   "}
}
