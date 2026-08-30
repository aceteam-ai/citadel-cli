package pairingdisplay

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitizeText_StripsControlAndANSIBytes(t *testing.T) {
	// SanitizeText strips control bytes only (0x00-0x1F, 0x7F) -- including
	// ESC (0x1B), the byte that INTRODUCES every ANSI/CSI escape sequence.
	// A terminal cannot interpret "[2J[H" as a clear-screen command without
	// the leading ESC: removing ESC alone is what neutralizes the sequence,
	// even though the printable bracket/digit/letter bytes that followed it
	// are themselves ordinary printable ASCII and are left as inert literal
	// text, same as any other printable character in requested_by.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "Agent Ops for jane", "Agent Ops for jane"},
		{"embedded ESC sequence neutralized (ESC removed, rest inert)", "evil\x1b[2J\x1b[Hname", "evil[2J[Hname"},
		{"leading control bytes stripped", "\x07\x07bell", "bell"},
		{"DEL stripped", "a\x7fb", "ab"},
		{"tab and newline stripped", "a\tb\nc", "abc"},
		{"UTF-8 continuation bytes untouched", "café équipe", "café équipe"},
		{"ESC bytes removed, printable remainder survives", "\x1b[2J\x1b[H", "[2J[H"},
		{"bare ESC removed", "\x1b\x1b\x1b", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeText(tc.in)
			if got != tc.want {
				t.Fatalf("SanitizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRenderShowFrame_SanitizesRequestedByBeforeRendering(t *testing.T) {
	req := ShowRequest{
		Code:        "12345678",
		RequestedBy: "evil\x1b[2J\x1b[Hname",
		ExpiresAt:   time.Now().Add(time.Minute),
		TTL:         time.Minute,
	}
	frame := renderShowFrame(req)
	if strings.Contains(frame, "\x1b[2J\x1b[Hname") {
		t.Fatalf("expected the ESC bytes to be stripped from requested_by (a second literal ESC would be a real leak), got %q", frame)
	}
	if !strings.Contains(frame, "evil[2J[Hname") {
		t.Fatalf("expected the sanitized (ESC-stripped, otherwise intact) requested_by text to still be rendered, got %q", frame)
	}
}

func TestRenderShowFrame_CodeIsRenderedLast(t *testing.T) {
	req := ShowRequest{
		Code:        "12345678",
		RequestedBy: "Agent Ops",
		ExpiresAt:   time.Now().Add(time.Minute),
		TTL:         time.Minute,
	}
	frame := renderShowFrame(req)

	reqIdx := strings.Index(frame, "Requested by")
	expiryIdx := strings.Index(frame, "Valid until")
	codeIdx := strings.Index(frame, bigDigits(req.Code))
	if reqIdx == -1 || expiryIdx == -1 || codeIdx == -1 {
		t.Fatalf("expected requested-by, expiry, and code block all present: reqIdx=%d expiryIdx=%d codeIdx=%d in %q",
			reqIdx, expiryIdx, codeIdx, frame)
	}
	if !(reqIdx < codeIdx && expiryIdx < codeIdx) {
		t.Fatalf("expected the code block to be rendered AFTER both the requested-by and expiry lines "+
			"(so a free-text field can never clobber it via an escape sequence), got reqIdx=%d expiryIdx=%d codeIdx=%d",
			reqIdx, expiryIdx, codeIdx)
	}
}

func TestRenderShowFrame_MaliciousRequestedByCannotEraseCode(t *testing.T) {
	// A requested_by that -- absent sanitization -- is itself a full
	// clear-screen-and-home sequence: if it survived into the frame
	// unsanitized, the frame would contain the clear sequence TWICE (once
	// from the frame's own header, once from the injected value), and
	// could erase the just-rendered code on a real terminal. Stripping the
	// ESC byte alone is sufficient to neutralize it: "[2J[H" without a
	// leading ESC is inert literal text to any terminal, not a command.
	req := ShowRequest{
		Code:        "48213097",
		RequestedBy: "\x1b[2J\x1b[H",
		ExpiresAt:   time.Now().Add(time.Minute),
		TTL:         time.Minute,
	}
	frame := renderShowFrame(req)

	if got := strings.Count(frame, ansiClearHome); got != 1 {
		t.Fatalf("expected the ANSI clear-home sequence (ESC+[2J[H) to appear exactly once (the frame's own header) -- "+
			"a second occurrence would mean the ESC byte survived sanitization -- got %d times in %q", got, frame)
	}
	if !strings.Contains(frame, bigDigits(req.Code)) {
		t.Fatalf("expected the code's block-digit rendering to be present, got %q", frame)
	}
	// The sanitized (ESC-stripped) remainder is inert printable text, so it
	// is still rendered on the Requested-by line -- unlike an
	// all-control-byte value (tested separately below), this one is not
	// empty after sanitization.
	if !strings.Contains(frame, "Requested by: [2J[H") {
		t.Fatalf("expected the sanitized remainder to render as inert text, got %q", frame)
	}
}

func TestRenderShowFrame_AllControlByteRequestedByOmitsTheLine(t *testing.T) {
	req := ShowRequest{
		Code:        "48213097",
		RequestedBy: "\x07\x1b\x7f",
		ExpiresAt:   time.Now().Add(time.Minute),
		TTL:         time.Minute,
	}
	frame := renderShowFrame(req)
	if strings.Contains(frame, "Requested by") {
		t.Fatalf("expected no Requested-by line when the value sanitizes to empty, got %q", frame)
	}
}

func TestGlyphFor_NonPrintableASCIIFallsBackToPlaceholder(t *testing.T) {
	// Defense in depth beyond the worker-layer isPrintableASCII gate: even
	// if a future/alternate caller of Manager.Show skipped that
	// validation, glyphFor must never embed a raw control byte or non-ASCII
	// rune verbatim into the console output via the code field.
	glyph := glyphFor('\x1b')
	for _, row := range glyph {
		if strings.ContainsRune(row, '\x1b') {
			t.Fatalf("expected ESC to be substituted with a placeholder, got row %q", row)
		}
	}
	if !strings.Contains(glyph[1], "?") {
		t.Fatalf("expected the placeholder glyph to contain '?', got %+v", glyph)
	}
}

func TestTruncateTrim_DoesNotSplitMultiByteRune(t *testing.T) {
	// "é" is 2 bytes in UTF-8; truncating at a byte count that lands mid-rune
	// must back off to the previous full rune rather than emit a corrupt
	// trailing byte sequence.
	s := strings.Repeat("a", 9) + "é" // 9 ASCII bytes + 2-byte rune = 11 bytes
	got := truncateTrim(s, 10)        // byte 10 lands inside the 2-byte rune
	if !utf8.ValidString(got) {
		t.Fatalf("truncateTrim produced invalid UTF-8: %q", got)
	}
	if got != strings.Repeat("a", 9) {
		t.Fatalf("expected truncation to back off before the split rune, got %q", got)
	}
}
