package laneop

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ScreenKind is what the recognizer could prove about a pane's visible screen.
type ScreenKind int

const (
	// ScreenUnknown means no recognizer matched. A send refuses with
	// unknown_screen; a capture reports pane_busy as unknown.
	ScreenUnknown ScreenKind = iota
	// ScreenPrompt is a recognized harness input box. Draft holds its text.
	ScreenPrompt
	// ScreenDialog is a visible choice prompt. Enter would select its
	// highlighted option, so a send always refuses with human_prompt_open.
	ScreenDialog
)

// Screen is the recognizer's verdict over one plain-text capture.
type Screen struct {
	Kind  ScreenKind
	Draft string
	Busy  bool
}

const (
	// screenRegion bounds how far up from the last non-empty line the
	// recognizer looks. Everything above it is transcript, never input.
	screenRegion = 30
	// maxFrameLines bounds the input box's content lines.
	maxFrameLines = 12
	// maxFooterLines bounds the hint lines a harness draws under its box.
	maxFooterLines = 6
	minRuleRunes   = 8
)

var (
	// A numbered option under a selection marker: a permission or approval
	// dialog. Matching transcript text that merely looks like one only makes
	// a send refuse, which is the safe direction.
	dialogOption = regexp.MustCompile(`^[│┃|]?\s*[❯›>]\s*[0-9]{1,2}\.\s`)
	// A large paste that the harness collapses into a placeholder.
	pastedMarker = regexp.MustCompile(`^\[Pasted text #[0-9]{1,4}(?: \+[0-9]{1,6} lines)?\]$`)
)

// ReadScreen recognizes a framed harness input box: a horizontal rule (or box
// top), content lines whose first line starts with a prompt glyph, and a rule
// (or box bottom) under it, with at most a few hint lines below. It is a
// label and layout heuristic pinned by fixtures in screen_test.go, not a
// terminal emulator. Anything it does not recognize is ScreenUnknown. Known
// limit: a dim placeholder hint inside an empty box reads as a draft, because
// a plain capture carries no attributes; that refuses, never types over it.
func ReadScreen(text string) Screen {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRightFunc(lines[i], unicode.IsSpace)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start := len(lines) - screenRegion
	if start < 0 {
		start = 0
	}
	region := lines[start:]
	var screen Screen
	for _, line := range region {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "esc to interrupt") {
			screen.Busy = true
		}
		if dialogOption.MatchString(strings.TrimSpace(line)) || strings.Contains(line, "Do you want to ") {
			screen.Kind = ScreenDialog
		}
	}
	if screen.Kind == ScreenDialog {
		return screen
	}
	for bottom := len(region) - 1; bottom >= 0 && len(region)-1-bottom <= maxFooterLines; bottom-- {
		if !isRule(region[bottom]) {
			continue
		}
		draft, ok := frameAbove(region, bottom)
		if ok {
			screen.Kind = ScreenPrompt
			screen.Draft = draft
			return screen
		}
	}
	return screen
}

func frameAbove(region []string, bottom int) (string, bool) {
	top := -1
	for j := bottom - 1; j >= 0 && bottom-j <= maxFrameLines+1; j-- {
		if isRule(region[j]) {
			top = j
			break
		}
	}
	if top < 0 || bottom-top < 2 {
		return "", false
	}
	content := region[top+1 : bottom]
	first := stripBorder(content[0])
	glyph, size := utf8.DecodeRuneInString(first)
	if glyph != '>' && glyph != '❯' && glyph != '›' {
		return "", false
	}
	rest := first[size:]
	if rest != "" {
		next, _ := utf8.DecodeRuneInString(rest)
		if !unicode.IsSpace(next) {
			return "", false
		}
	}
	parts := []string{strings.TrimSpace(rest)}
	for _, line := range content[1:] {
		parts = append(parts, stripBorder(line))
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), true
}

func stripBorder(line string) string {
	line = strings.TrimSpace(line)
	for _, border := range []string{"│", "┃", "|"} {
		line = strings.TrimPrefix(line, border)
		line = strings.TrimSuffix(line, border)
	}
	return strings.TrimSpace(line)
}

func isRule(line string) bool {
	runes := []rune(strings.TrimSpace(line))
	if len(runes) < minRuleRunes {
		return false
	}
	if strings.ContainsRune("╭╰┌└├", runes[0]) {
		runes = runes[1:]
	}
	if n := len(runes); n > 0 && strings.ContainsRune("╮╯┐┘┤", runes[n-1]) {
		runes = runes[:n-1]
	}
	for _, r := range runes {
		if !strings.ContainsRune("─━═╌┄", r) {
			return false
		}
	}
	return len(runes) > 0
}

// draftHolds reports whether the input box now holds exactly message, or the
// placeholder a harness shows for a collapsed paste. Whitespace is ignored on
// both sides because the box re-wraps and indents long text.
func draftHolds(draft, message string) bool {
	if pastedMarker.MatchString(draft) {
		return true
	}
	return squash(draft) != "" && squash(draft) == squash(message)
}

func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
