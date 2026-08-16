package controlcenter

import (
	"os"
	"strings"
)

// This file is the single source of truth for the status glyphs the Control
// Center panes render (header, node/worker status, action list, peers table,
// activity feed, and the various detail/modal views). citadel #656: the
// Proxmox noVNC console (and other consoles without an emoji-capable font)
// renders these symbols as bare "?", turning the whole screen into a wall of
// question marks on a new operator's very first look. Every pane must go
// through Glyph (or one of the Marker* helpers below) instead of hardcoding
// a Unicode symbol, so the emoji-vs-ASCII choice lives in exactly one place.
//
// Box-drawing characters (─│═╔╗╚╝║) are deliberately NOT covered here — the
// issue confirmed those render fine even where the status glyphs fail, so
// treating them as part of "emoji" would be scope creep without a matching
// bug report.

// Marker identifies a semantic status glyph.
type Marker int

const (
	// MarkerActive is a filled "on"/"running"/"online" dot.
	MarkerActive Marker = iota
	// MarkerInactive is a hollow "off"/"stopped"/"offline" dot.
	MarkerInactive
	// MarkerConnecting is an in-progress/transitional dot.
	MarkerConnecting
	// MarkerBullet is a plain list bullet.
	MarkerBullet
	// MarkerOK marks success/trusted/done.
	MarkerOK
	// MarkerWarn marks a warning or an untrusted/needs-attention state.
	MarkerWarn
	// MarkerError marks failure/error.
	MarkerError
	// MarkerBolt decorates the header title.
	MarkerBolt
	// MarkerArrowUp is the "move selection up" hint glyph.
	MarkerArrowUp
	// MarkerArrowDown is the "move selection down" hint glyph.
	MarkerArrowDown
	// MarkerArrowBoth separates two things focus cycles between.
	MarkerArrowBoth
	// MarkerPointer marks "look here" (e.g. "scan the QR ➜").
	MarkerPointer
	// MarkerThreadReply prefixes a threaded chat reply.
	MarkerThreadReply
)

// glyphSet is the emoji-capable glyph paired with its ASCII fallback.
type glyphSet struct {
	emoji string
	ascii string
}

var glyphTable = map[Marker]glyphSet{
	MarkerActive:      {emoji: "●", ascii: "*"},
	MarkerInactive:    {emoji: "○", ascii: "-"},
	MarkerConnecting:  {emoji: "◐", ascii: "~"},
	MarkerBullet:      {emoji: "•", ascii: "-"},
	MarkerOK:          {emoji: "✓", ascii: "OK"},
	MarkerWarn:        {emoji: "⚠", ascii: "!"},
	MarkerError:       {emoji: "✗", ascii: "X"},
	MarkerBolt:        {emoji: "⚡", ascii: "*"},
	MarkerArrowUp:     {emoji: "↑", ascii: "^"},
	MarkerArrowDown:   {emoji: "↓", ascii: "v"},
	MarkerArrowBoth:   {emoji: "↔", ascii: "<->"},
	MarkerPointer:     {emoji: "➜", ascii: "->"},
	MarkerThreadReply: {emoji: "↳", ascii: "->"},
}

// Glyph returns the marker's rune, substituting the ASCII fallback when the
// terminal is unlikely to render the emoji/symbol form (see UseASCIIGlyphs).
// Unknown markers return "?" rather than panicking — a bug in a call site
// should degrade, not crash the TUI.
func Glyph(m Marker) string {
	set, ok := glyphTable[m]
	if !ok {
		return "?"
	}
	if UseASCIIGlyphs() {
		return set.ascii
	}
	return set.emoji
}

// UseASCIIGlyphs reports whether the Control Center should render ASCII
// status markers instead of emoji/symbol glyphs. It is recomputed on every
// call (env lookups are cheap and this keeps tests trivial to drive) with
// this precedence:
//
//  1. CITADEL_ASCII or NO_EMOJI, if truthy/falsy, wins outright.
//  2. CITADEL_EMOJI, if truthy/falsy, wins next (explicit opt back into
//     emoji on a terminal that would otherwise be auto-detected as limited).
//  3. Otherwise, auto-detect via detectLimitedTerminal.
func UseASCIIGlyphs() bool {
	if v, ok := parseBoolEnv("CITADEL_ASCII"); ok {
		return v
	}
	if v, ok := parseBoolEnv("NO_EMOJI"); ok {
		return v
	}
	if v, ok := parseBoolEnv("CITADEL_EMOJI"); ok {
		return !v
	}
	return detectLimitedTerminal()
}

// detectLimitedTerminal guesses, from TERM and the locale environment
// variables, whether the terminal is unlikely to have emoji/symbol glyphs
// available. This is deliberately conservative: per citadel #656, a
// misdetection toward ASCII costs nothing (plain markers are always
// readable), while a misdetection toward emoji renders the whole screen as
// "?". Signals, in order:
//
//   - TERM unset, "dumb", or "linux" (the Linux virtual-console/framebuffer
//     font — the common case behind a Proxmox noVNC/serial console — ships a
//     minimal glyph set with no emoji or box-drawing dingbats even though it
//     is perfectly capable of UTF-8 text).
//   - The active locale (LC_ALL, then LC_CTYPE, then LANG — the standard
//     POSIX precedence) is not a UTF-8 locale. Note #656 specifically found
//     a UTF-8 locale with a broken *font* is not caught by this signal alone
//     — that's what the TERM check above is for.
//   - No locale variable is set at all: nothing to go on, so default to
//     ASCII rather than assume emoji support.
func detectLimitedTerminal() bool {
	switch os.Getenv("TERM") {
	case "", "dumb", "linux":
		return true
	}

	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(key); v != "" {
			upper := strings.ToUpper(v)
			return !strings.Contains(upper, "UTF-8") && !strings.Contains(upper, "UTF8")
		}
	}

	// No locale info at all: when in doubt, ASCII is the safe default.
	return true
}

// parseBoolEnv reads a truthy/falsy env var. ok is false when the variable
// is unset or empty, meaning "no opinion" — callers should fall through to
// the next signal.
func parseBoolEnv(key string) (value bool, ok bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return false, false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		// Unrecognized value: treat as unset rather than guessing.
		return false, false
	}
}
