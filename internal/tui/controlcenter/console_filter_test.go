package controlcenter

import "testing"

// filterString is a test helper that runs the filter over a single chunk.
func filterString(f *consoleFilter, s string) string {
	return string(f.filter([]byte(s)))
}

func TestConsoleFilterStripsCharsetDesignation(t *testing.T) {
	var f consoleFilter
	// The reported bug: ESC(B (designate G0 = US-ASCII) leaks its final "B".
	got := filterString(&f, "\x1b(Bjason\x1b(B@host")
	want := "jason@host"
	if got != want {
		t.Errorf("filter = %q, want %q", got, want)
	}
}

func TestConsoleFilterAllCharsetIntermediates(t *testing.T) {
	var f consoleFilter
	// ESC ( ) * + each followed by a final byte must all be consumed.
	got := filterString(&f, "a\x1b(Bb\x1b)0c\x1b*Ad\x1b+Be")
	want := "abcde"
	if got != want {
		t.Errorf("filter = %q, want %q", got, want)
	}
}

func TestConsoleFilterSplitAcrossChunks(t *testing.T) {
	var f consoleFilter
	// ESC( at the tail of one chunk, B at the head of the next.
	if got := filterString(&f, "\x1b("); got != "" {
		t.Errorf("first chunk = %q, want empty", got)
	}
	if got := filterString(&f, "Bjason"); got != "jason" {
		t.Errorf("second chunk = %q, want %q", got, "jason")
	}
}

func TestConsoleFilterSplitEscOnly(t *testing.T) {
	var f consoleFilter
	// Lone ESC at end of chunk, then intermediate+final next chunk.
	if got := filterString(&f, "hi\x1b"); got != "hi" {
		t.Errorf("first chunk = %q, want %q", got, "hi")
	}
	if got := filterString(&f, "(0world"); got != "world" {
		t.Errorf("second chunk = %q, want %q", got, "world")
	}
}

func TestConsoleFilterPreservesCSI(t *testing.T) {
	var f consoleFilter
	// CSI color sequences must pass through untouched for tview to parse.
	in := "\x1b[31mred\x1b[0m"
	if got := filterString(&f, in); got != in {
		t.Errorf("filter = %q, want %q (CSI must be preserved)", got, in)
	}
}

func TestConsoleFilterPreservesOSC(t *testing.T) {
	var f consoleFilter
	// OSC (ESC]) title sequences must pass through untouched.
	in := "\x1b]0;title\x07text"
	if got := filterString(&f, in); got != in {
		t.Errorf("filter = %q, want %q (OSC must be preserved)", got, in)
	}
}

func TestConsoleFilterStripsCarriageReturn(t *testing.T) {
	var f consoleFilter
	// CRLF collapses to LF; a stray CR is dropped.
	if got := filterString(&f, "line1\r\nline2\r\n"); got != "line1\nline2\n" {
		t.Errorf("filter = %q, want %q", got, "line1\nline2\n")
	}
}

func TestConsoleFilterCarriageReturnSplit(t *testing.T) {
	var f consoleFilter
	// CR at end of chunk, LF at start of next: still one line break.
	if got := filterString(&f, "abc\r"); got != "abc" {
		t.Errorf("first chunk = %q, want %q", got, "abc")
	}
	if got := filterString(&f, "\ndef"); got != "\ndef" {
		t.Errorf("second chunk = %q, want %q", got, "\ndef")
	}
}

func TestConsoleFilterPlainText(t *testing.T) {
	var f consoleFilter
	in := "just some plain text 123"
	if got := filterString(&f, in); got != in {
		t.Errorf("filter = %q, want %q", got, in)
	}
}

func TestConsoleFilterRealisticPrompt(t *testing.T) {
	var f consoleFilter
	// Approximates the prompt from the bug report:
	// "BjasonB@Bubuntu-gpuB ~BB>" was the corrupted output.
	in := "\x1b(Bjason\x1b(B@\x1b(Bubuntu-gpu\x1b(B ~\x1b(B\x1b(B>"
	want := "jason@ubuntu-gpu ~>"
	if got := filterString(&f, in); got != want {
		t.Errorf("filter = %q, want %q", got, want)
	}
}
