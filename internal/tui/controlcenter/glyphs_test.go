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
