package controlcenter

import (
	"strings"
	"testing"
)

// stripSGR removes SGR colour, charset and other escape prefixes so a test can
// assert on the visible characters of the rendered line independent of colour.
func stripSGR(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			// Skip ESC [ ... final, ESC ] ... BEL, or ESC ( x.
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && !isCSIFinal(s[j]) {
					j++
				}
				i = j + 1
				continue
			}
			if i+1 < len(s) && (s[i+1] == '(' || s[i+1] == ')' || s[i+1] == '*' || s[i+1] == '+') {
				i += 3
				continue
			}
			if i+1 < len(s) && s[i+1] == ']' {
				j := i + 2
				for j < len(s) && s[j] != 0x07 {
					j++
				}
				i = j + 1
				continue
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// visibleLastLine returns the visible text of the final rendered line.
func visibleLastLine(rendered string) string {
	v := stripSGR(rendered)
	lines := strings.Split(v, "\n")
	return lines[len(lines)-1]
}

// TestLineEmulatorFishTypingLS feeds the EXACT bytes captured from fish 3.7.0
// (TERM=xterm-256color) when typing 'l', then 's', then ' '. Without emulation
// the old filter produced `llss`-style accumulation; the emulator must collapse
// each in-place repaint to the real prompt + input.
//
// The chunks below are verbatim from the characterization harness
// (creack/pty spawning `fish -i`).
func TestLineEmulatorFishTypingLS(t *testing.T) {
	var e lineEmulator

	// Drive the prompt draw (final startup chunk) so the cursor sits after the
	// prompt, exactly as fish leaves it before the user types.
	// prompt: "...jason@ubuntu-gpu /t/c/-/5/s/fishcap> " then CR + forward 37.
	e.feed([]byte("\x1b[92mjason\x1b(B\x1b[m@\x1b(B\x1b[mubuntu-gpu\x1b(B\x1b[m \x1b[32m/t/c/-/5/s/fishcap\x1b(B\x1b[m\x1b(B\x1b[m> \x1b[K\r\x1b[37C"))

	// Type 'l': echo l, CR, forward 38; then BS + recolour l + CR + fwd 38;
	// then autosuggestion 's' (gray) + CR + fwd 38.
	e.feed([]byte("l\r\x1b[38C"))
	e.feed([]byte("\b\x1b[91ml\x1b[30m\x1b(B\x1b[m\r\x1b[38C"))
	out := e.feed([]byte("\x1b[90ms\x1b[30m\x1b(B\x1b[m\r\x1b[38C"))

	got := visibleLastLine(out)
	// After typing 'l', the visible line is the prompt, then "l", then the
	// gray autosuggestion "s": "...> ls". Crucially it is NOT "ll" or "lls".
	if !strings.HasSuffix(got, "> ls") {
		t.Fatalf("after typing 'l': visible line = %q, want suffix %q", got, "> ls")
	}

	// Type 's': recolour s + CR + fwd 39 (twice); then 2x BS + redraw "ls".
	e.feed([]byte("\x1b[91ms\x1b[30m\x1b(B\x1b[m\r\x1b[39C\r\x1b[39C"))
	out = e.feed([]byte("\b\b\x1b[34mls\x1b[30m\x1b(B\x1b[m\r\x1b[39C"))
	got = visibleLastLine(out)
	if !strings.HasSuffix(got, "> ls") {
		t.Fatalf("after typing 's': visible line = %q, want suffix %q (NOT llss)", got, "> ls")
	}
	if strings.Contains(got, "llss") || strings.Contains(got, "lls") {
		t.Fatalf("double-echo regression: visible line = %q contains doubled chars", got)
	}

	// Type ' ': redraw " " then BS + space + CR + fwd 40, then a gray history
	// autosuggestion. The committed input remains "ls ".
	e.feed([]byte("\x1b[34m \x1b[30m\x1b(B\x1b[m\r\x1b[40C"))
	e.feed([]byte("\b \r\x1b[40C"))
	out = e.feed([]byte("\x1b[90m~/workspace/sunapi386/private/trading/\xe2\x80\xa6\x1b[30m\x1b(B\x1b[m\r\x1b[40C"))
	got = visibleLastLine(out)
	if !strings.Contains(got, "> ls ") {
		t.Fatalf("after typing space: visible line = %q, want to contain %q", got, "> ls ")
	}
	if strings.Contains(got, "ll") || strings.Contains(got, "ss") {
		t.Fatalf("double-echo regression: visible line = %q contains doubled chars", got)
	}
}

// TestLineEmulatorCarriageReturnOverwrite is the minimal proof of the fix: a CR
// followed by new text overwrites from column 0 instead of appending.
func TestLineEmulatorCarriageReturnOverwrite(t *testing.T) {
	var e lineEmulator
	out := e.feed([]byte("hello\rworld"))
	if got := visibleLastLine(out); got != "world" {
		t.Fatalf("CR overwrite: got %q, want %q", got, "world")
	}
}

// TestLineEmulatorBackspace verifies BS moves the cursor so the next write
// overwrites the previous char.
func TestLineEmulatorBackspace(t *testing.T) {
	var e lineEmulator
	out := e.feed([]byte("abc\b\bX"))
	if got := visibleLastLine(out); got != "aXc" {
		t.Fatalf("backspace overwrite: got %q, want %q", got, "aXc")
	}
}

// TestLineEmulatorEraseToEOL verifies CSI K truncates at the cursor.
func TestLineEmulatorEraseToEOL(t *testing.T) {
	var e lineEmulator
	// Write "abcdef", CR to col0, forward 3, erase to end -> "abc".
	out := e.feed([]byte("abcdef\r\x1b[3C\x1b[K"))
	if got := visibleLastLine(out); got != "abc" {
		t.Fatalf("erase to EOL: got %q, want %q", got, "abc")
	}
}

// TestLineEmulatorCursorForwardPads verifies CSI nC past end-of-line pads with
// spaces (fish skips over the prompt this way).
func TestLineEmulatorCursorForwardPads(t *testing.T) {
	var e lineEmulator
	out := e.feed([]byte("\x1b[3CX"))
	if got := visibleLastLine(out); got != "   X" {
		t.Fatalf("cursor forward pad: got %q, want %q", got, "   X")
	}
}

// TestLineEmulatorLineFeedCommits verifies LF commits the current line and a
// later CR only affects the new line.
func TestLineEmulatorLineFeedCommits(t *testing.T) {
	var e lineEmulator
	out := e.feed([]byte("line1\nline2\rX"))
	v := stripSGR(out)
	if v != "line1\nXine2" {
		t.Fatalf("linefeed commit: got %q, want %q", v, "line1\nXine2")
	}
}

// TestLineEmulatorCharsetStrippedAndColourKept verifies #296 charset escapes are
// consumed while SGR colour passes through (visible text excludes the charset
// finals like the leaked "B").
func TestLineEmulatorCharsetStrippedAndColourKept(t *testing.T) {
	var e lineEmulator
	out := e.feed([]byte("\x1b(Bjason\x1b(B@\x1b(Bhost"))
	if got := visibleLastLine(out); got != "jason@host" {
		t.Fatalf("charset strip: got %q, want %q", got, "jason@host")
	}
	// Colour must be preserved in the raw (un-stripped) output.
	coloured := e.render()
	_ = coloured
	var e2 lineEmulator
	raw := e2.feed([]byte("\x1b[31mred\x1b[0m"))
	if !strings.Contains(raw, "\x1b[31m") {
		t.Fatalf("SGR colour must be preserved, got %q", raw)
	}
}

// TestLineEmulatorSplitEscapeAcrossChunks verifies a CSI sequence split over a
// read-chunk boundary is resolved correctly (the PTY is read in fixed chunks).
func TestLineEmulatorSplitEscapeAcrossChunks(t *testing.T) {
	var e lineEmulator
	// "abc", then a CR + cursor-forward split: "\r\x1b[" in chunk 1, "2C" + "X"
	// in chunk 2 -> overwrite at column 2.
	e.feed([]byte("abc\r\x1b["))
	out := e.feed([]byte("2CX"))
	if got := visibleLastLine(out); got != "abX" {
		t.Fatalf("split escape: got %q, want %q", got, "abX")
	}
}

// TestLineEmulatorSplitRuneAcrossChunks verifies a multi-byte UTF-8 rune split
// across chunks renders once and correctly.
func TestLineEmulatorSplitRuneAcrossChunks(t *testing.T) {
	var e lineEmulator
	// "é" is 0xc3 0xa9; split it.
	e.feed([]byte{'a', 0xc3})
	out := e.feed([]byte{0xa9, 'b'})
	if got := visibleLastLine(out); got != "aéb" {
		t.Fatalf("split rune: got %q, want %q", got, "aéb")
	}
}
