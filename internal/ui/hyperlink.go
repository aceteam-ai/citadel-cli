// internal/ui/hyperlink.go
package ui

import "fmt"

// Hyperlink creates a clickable hyperlink using OSC 8 escape sequences.
// This works in most modern terminal emulators (iTerm2, Windows Terminal,
// GNOME Terminal, Konsole, etc.).
// The returned string displays `text` but clicking opens `url`.
func Hyperlink(url, text string) string {
	// OSC 8 format: \x1b]8;;URL\x07TEXT\x1b]8;;\x07
	return fmt.Sprintf("\x1b]8;;%s\x07%s\x1b]8;;\x07", url, text)
}

// HyperlinkSelf creates a clickable hyperlink where the URL is displayed as-is.
func HyperlinkSelf(url string) string {
	return Hyperlink(url, url)
}
