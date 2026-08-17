package controlcenter

import (
	"testing"
)

// withEnv sets the given env vars for the duration of the test and restores
// their previous values (including "unset") afterward via t.Cleanup.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// clearGlyphEnv unsets every env var the detection logic reads, so each test
// starts from a clean slate regardless of the ambient shell/CI environment.
func clearGlyphEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"CITADEL_ASCII", "NO_EMOJI", "CITADEL_EMOJI", "TERM", "LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
	}
}

func TestUseASCIIGlyphs_ExplicitOverrides(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "CITADEL_ASCII=1 forces ascii even on a UTF-8 xterm",
			env:  map[string]string{"CITADEL_ASCII": "1", "TERM": "xterm-256color", "LANG": "en_US.UTF-8"},
			want: true,
		},
		{
			name: "CITADEL_ASCII=0 forces emoji even on a bare TERM",
			env:  map[string]string{"CITADEL_ASCII": "0", "TERM": "linux"},
			want: false,
		},
		{
			name: "NO_EMOJI=true forces ascii",
			env:  map[string]string{"NO_EMOJI": "true", "TERM": "xterm-256color", "LANG": "en_US.UTF-8"},
			want: true,
		},
		{
			name: "NO_EMOJI=false forces emoji",
			env:  map[string]string{"NO_EMOJI": "false", "TERM": "linux"},
			want: false,
		},
		{
			name: "CITADEL_EMOJI=1 opts back into emoji on an otherwise-limited terminal",
			env:  map[string]string{"CITADEL_EMOJI": "1", "TERM": "linux"},
			want: false,
		},
		{
			name: "CITADEL_EMOJI=0 forces ascii on an otherwise-capable terminal",
			env:  map[string]string{"CITADEL_EMOJI": "0", "TERM": "xterm-256color", "LANG": "en_US.UTF-8"},
			want: true,
		},
		{
			name: "CITADEL_ASCII wins over CITADEL_EMOJI when both set",
			env:  map[string]string{"CITADEL_ASCII": "1", "CITADEL_EMOJI": "1", "TERM": "xterm-256color", "LANG": "en_US.UTF-8"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGlyphEnv(t)
			withEnv(t, tt.env)
			if got := UseASCIIGlyphs(); got != tt.want {
				t.Errorf("UseASCIIGlyphs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUseASCIIGlyphs_AutoDetect exercises the citadel #656 scenario directly:
// a container that is genuinely en_US.UTF-8 (so a naive locale-only check
// would call it emoji-capable) but whose console TERM is the limited Linux
// virtual-console font — the case that motivated this file.
func TestUseASCIIGlyphs_AutoDetect(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{
			name: "no TERM, no locale: safe default is ascii",
			env:  map[string]string{},
			want: true,
		},
		{
			name: "TERM=dumb is always ascii",
			env:  map[string]string{"TERM": "dumb", "LANG": "en_US.UTF-8"},
			want: true,
		},
		{
			name: "TERM=linux is ascii even with a genuine UTF-8 locale (citadel #656)",
			env:  map[string]string{"TERM": "linux", "LANG": "en_US.UTF-8"},
			want: true,
		},
		{
			name: "TERM=xterm-256color with UTF-8 locale is emoji-capable",
			env:  map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"},
			want: false,
		},
		{
			name: "TERM=xterm-256color with a non-UTF-8 locale falls back to ascii",
			env:  map[string]string{"TERM": "xterm-256color", "LANG": "C"},
			want: true,
		},
		{
			name: "LC_ALL takes precedence over LANG",
			env:  map[string]string{"TERM": "xterm-256color", "LC_ALL": "C", "LANG": "en_US.UTF-8"},
			want: true,
		},
		{
			name: "LC_CTYPE takes precedence over LANG when LC_ALL unset",
			env:  map[string]string{"TERM": "xterm-256color", "LC_CTYPE": "en_US.UTF-8", "LANG": "C"},
			want: false,
		},
		{
			name: "utf8 spelling (no hyphen) is also recognized",
			env:  map[string]string{"TERM": "xterm-256color", "LANG": "en_US.utf8"},
			want: false,
		},
		{
			name: "TERM set but no locale vars at all: safe default is ascii",
			env:  map[string]string{"TERM": "xterm-256color"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGlyphEnv(t)
			withEnv(t, tt.env)
			if got := UseASCIIGlyphs(); got != tt.want {
				t.Errorf("UseASCIIGlyphs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGlyph_SwitchesOnMode(t *testing.T) {
	tests := []struct {
		marker    Marker
		wantEmoji string
		wantASCII string
	}{
		{MarkerActive, "●", "*"},
		{MarkerInactive, "○", "-"},
		{MarkerOK, "✓", "OK"},
		{MarkerWarn, "⚠", "!"},
		{MarkerError, "✗", "X"},
		{MarkerArrowUp, "↑", "^"},
		{MarkerArrowDown, "↓", "v"},
		{MarkerCheckbox, "✓", "x"},
	}

	for _, tt := range tests {
		clearGlyphEnv(t)
		withEnv(t, map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"})
		if got := Glyph(tt.marker); got != tt.wantEmoji {
			t.Errorf("Glyph(%v) in emoji mode = %q, want %q", tt.marker, got, tt.wantEmoji)
		}

		clearGlyphEnv(t)
		withEnv(t, map[string]string{"CITADEL_ASCII": "1"})
		if got := Glyph(tt.marker); got != tt.wantASCII {
			t.Errorf("Glyph(%v) in ascii mode = %q, want %q", tt.marker, got, tt.wantASCII)
		}
	}
}

// TestGlyph_UnknownMarkerDegradesGracefully ensures a bug at a call site
// (an out-of-range Marker) renders a visible "?" instead of panicking the
// whole Control Center.
func TestGlyph_UnknownMarkerDegradesGracefully(t *testing.T) {
	clearGlyphEnv(t)
	const bogus Marker = 9999
	if got := Glyph(bogus); got != "?" {
		t.Errorf("Glyph(bogus) = %q, want %q", got, "?")
	}
}

// TestPadGlyph verifies the fixed-width table-alignment helper: a no-op in
// emoji mode (every glyph is already 1 rune), space-padding in ASCII mode
// where fallbacks vary in rune length (e.g. "OK" vs "X" vs "*").
func TestPadGlyph(t *testing.T) {
	t.Run("emoji mode is a no-op regardless of width", func(t *testing.T) {
		clearGlyphEnv(t)
		withEnv(t, map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"})
		if got := PadGlyph(MarkerOK, 2); got != "✓" {
			t.Errorf("PadGlyph(MarkerOK, 2) in emoji mode = %q, want %q (no padding)", got, "✓")
		}
	})

	t.Run("ascii mode pads shorter glyphs to the requested width", func(t *testing.T) {
		clearGlyphEnv(t)
		withEnv(t, map[string]string{"CITADEL_ASCII": "1"})

		tests := []struct {
			marker Marker
			width  int
			want   string
		}{
			{MarkerOK, 2, "OK"},    // already at width: unchanged
			{MarkerError, 2, "X "}, // 1 rune -> padded to 2
			{MarkerActive, 2, "* "},
		}
		for _, tt := range tests {
			got := PadGlyph(tt.marker, tt.width)
			if got != tt.want {
				t.Errorf("PadGlyph(%v, %d) = %q, want %q", tt.marker, tt.width, got, tt.want)
			}
			if gotLen := len([]rune(got)); gotLen != tt.width {
				t.Errorf("PadGlyph(%v, %d) rune length = %d, want %d", tt.marker, tt.width, gotLen, tt.width)
			}
		}
	})

	t.Run("ascii mode never truncates a glyph wider than the requested width", func(t *testing.T) {
		clearGlyphEnv(t)
		withEnv(t, map[string]string{"CITADEL_ASCII": "1"})
		if got := PadGlyph(MarkerArrowBoth, 1); got != "<->" {
			t.Errorf("PadGlyph(MarkerArrowBoth, 1) = %q, want %q (never truncate)", got, "<->")
		}
	})
}

// TestJobStatusMarkersStayAlignedInASCIIMode is a regression test for the
// showJobsDetailModal recent-jobs table: success/failed/running rows must
// render the same total marker+word width in both modes, or the DURATION
// column that follows drifts on success rows (citadel #774 CI follow-up).
func TestJobStatusMarkersStayAlignedInASCIIMode(t *testing.T) {
	widthOf := func(marker Marker, word string) int {
		return len([]rune(PadGlyph(marker, 2))) + len([]rune(word))
	}

	for _, mode := range []struct {
		name string
		env  map[string]string
	}{
		{"emoji", map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"}},
		{"ascii", map[string]string{"CITADEL_ASCII": "1"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			clearGlyphEnv(t)
			withEnv(t, mode.env)

			success := widthOf(MarkerOK, " success")
			failed := widthOf(MarkerError, " failed ")
			running := widthOf(MarkerActive, " running")

			if success != failed || success != running {
				t.Errorf("marker+word widths not aligned: success=%d failed=%d running=%d", success, failed, running)
			}
		})
	}
}

func TestParseBoolEnv(t *testing.T) {
	tests := []struct {
		raw     string
		wantVal bool
		wantOK  bool
	}{
		{"", false, false},
		{"1", true, true},
		{"true", true, true},
		{"TRUE", true, true},
		{"yes", true, true},
		{"on", true, true},
		{"0", false, true},
		{"false", false, true},
		{"no", false, true},
		{"off", false, true},
		{"maybe", false, false},
		{"  1  ", true, true},
	}
	for _, tt := range tests {
		t.Run("raw="+tt.raw, func(t *testing.T) {
			t.Setenv("CITADEL_TEST_BOOL", tt.raw)
			val, ok := parseBoolEnv("CITADEL_TEST_BOOL")
			if val != tt.wantVal || ok != tt.wantOK {
				t.Errorf("parseBoolEnv(%q) = (%v, %v), want (%v, %v)", tt.raw, val, ok, tt.wantVal, tt.wantOK)
			}
		})
	}
}
