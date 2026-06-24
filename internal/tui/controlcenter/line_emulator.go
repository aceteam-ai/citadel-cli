package controlcenter

import "strconv"

// escByte is the ASCII escape control byte (ESC, 0x1b) that introduces terminal
// escape sequences.
const escByte = 0x1b

// lineEmulator is a minimal, single-line terminal emulator for the TUI Console.
//
// The Console renders PTY output into a line-oriented tview.TextView, which is
// NOT a terminal emulator: it has no cursor and only ever appends text. That is
// fine for programs that print forward, but interactive shells that repaint the
// current input line in place — notably fish, which redraws the whole command
// line on every keystroke for syntax highlighting and autosuggestions — emit
// carriage returns, backspaces, cursor-forward (CSI nC) and erase-to-EOL
// (CSI K) to overwrite what they already drew. Without a cursor, an append-only
// view turns each repaint into accumulation: typing `ls` renders as `llss`
// (issue: fish double-echo). See also #296 (charset escapes) and #307 (DA
// queries): the same class of "fake terminal chokes on a control sequence a
// real terminal handles".
//
// lineEmulator interprets exactly the cursor-affecting operations fish uses on
// the prompt line and produces the set of fully rendered lines. It is a pure,
// stateful transform (bytes in -> rendered lines out) so it can be table-tested
// against captured fish output with no PTY, tmux, or live TUI.
//
// SGR colour sequences (CSI ... m), charset-designation escapes (ESC ( B etc.),
// OSC sequences and other non-cursor escapes are preserved verbatim and attached
// to the cell they precede, so tview still renders colour correctly. Only the
// cursor-movement and erase operations are consumed.
//
// LIMITATIONS (unchanged pre-existing behaviour): this models a SINGLE logical
// line. Line wrapping when the prompt + input exceeds the terminal width, and
// full-screen applications (vim, htop, …) that move the cursor vertically, are
// not handled and remain the documented v1 limitation of the line-oriented
// Console. Swapping in a real tcell vt-emulator widget is future work.
type lineEmulator struct {
	// committed holds finished lines (everything before the cursor's current
	// line). These never change once committed by a line feed.
	committed []string

	// cells is the current line as a slice of cells. Each cell carries the
	// non-printing prefix (SGR/charset/etc.) that preceded its rune plus the
	// rune itself, so the line can be reconstructed with colour intact.
	cells []cell

	// col is the cursor column within the current line (0-based index into
	// cells; may equal len(cells) when at end-of-line).
	col int

	// pendingAttrs accumulates non-printing escape bytes (colour, charset, …)
	// seen since the last printable rune; they attach to the next printable
	// cell written.
	pendingAttrs []byte

	// carry holds a partial escape/CR straddling a read-chunk boundary, so a
	// sequence split across two PTY reads is resolved on the next call.
	carry []byte
}

// cell is one rendered column: the printable bytes (a single UTF-8 rune) plus
// any non-printing prefix (SGR/charset) that should be emitted before it.
type cell struct {
	attrs []byte // colour/charset prefix, may be empty
	r     []byte // the printable rune's UTF-8 bytes
}

// feed consumes a chunk of raw PTY bytes and updates the emulator state. It
// returns the full rendered output (committed lines + the current line) so the
// caller can hand it to the view. The returned string always ends without a
// trailing reset; colour state is whatever the shell last set.
func (e *lineEmulator) feed(input []byte) string {
	work := input
	if len(e.carry) > 0 {
		work = append(append(make([]byte, 0, len(e.carry)+len(input)), e.carry...), input...)
		e.carry = nil
	}

	i := 0
	for i < len(work) {
		b := work[i]
		switch {
		case b == '\n':
			e.lineFeed()
			i++

		case b == '\r':
			// Carriage return: move cursor to column 0 of the current line.
			// (We do NOT commit; fish uses CR to reposition before repainting.)
			e.col = 0
			i++

		case b == '\b':
			// Backspace: move cursor left one column.
			if e.col > 0 {
				e.col--
			}
			i++

		case b == 0x1b:
			consumed, partial := e.handleEscape(work[i:])
			if partial {
				e.carry = append([]byte(nil), work[i:]...)
				return e.render()
			}
			i += consumed

		case b < 0x20:
			// Other C0 control bytes (BEL, TAB, …) are not used by fish's
			// line repaint; drop them rather than corrupt the line. TAB in
			// particular would need width logic we don't model here.
			i++

		default:
			// Printable byte: decode one UTF-8 rune and write a cell.
			n := utf8Len(b)
			if i+n > len(work) {
				// Partial multi-byte rune at chunk end; carry it.
				e.carry = append([]byte(nil), work[i:]...)
				return e.render()
			}
			e.writeRune(work[i : i+n])
			i += n
		}
	}

	return e.render()
}

// handleEscape processes an escape sequence starting at s[0] == ESC. It returns
// the number of bytes consumed, or partial=true when the sequence is incomplete
// (straddles a chunk boundary) and should be carried to the next feed.
func (e *lineEmulator) handleEscape(s []byte) (consumed int, partial bool) {
	if len(s) < 2 {
		return 0, true
	}
	switch s[1] {
	case '[':
		// CSI: ESC [ params ... final. Find the final byte (0x40-0x7e).
		j := 2
		for j < len(s) && !isCSIFinal(s[j]) {
			j++
		}
		if j >= len(s) {
			return 0, true // final byte not yet available
		}
		final := s[j]
		params := string(s[2:j])
		seq := s[:j+1]
		e.applyCSI(final, params, seq)
		return j + 1, false

	case ']':
		// OSC: ESC ] ... terminated by BEL (0x07) or ST (ESC \). These set
		// titles etc.; they are not cursor ops and not visible text, so we
		// consume and discard them.
		j := 2
		for j < len(s) {
			if s[j] == 0x07 {
				return j + 1, false
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2, false
			}
			if s[j] == 0x1b && j+1 >= len(s) {
				return 0, true
			}
			j++
		}
		return 0, true // terminator not yet available

	case '(', ')', '*', '+':
		// Charset designation: ESC + intermediate + one final byte. Consume
		// and discard (see #296).
		if len(s) < 3 {
			return 0, true
		}
		return 3, false

	default:
		// Any other 2-byte escape: pass through as an attribute prefix so we
		// don't corrupt the stream, and consume both bytes.
		e.pendingAttrs = append(e.pendingAttrs, s[0], s[1])
		return 2, false
	}
}

// applyCSI handles a complete CSI sequence.
func (e *lineEmulator) applyCSI(final byte, params string, seq []byte) {
	switch final {
	case 'm':
		// SGR colour: keep as an attribute prefix attached to the next rune.
		e.pendingAttrs = append(e.pendingAttrs, seq...)

	case 'C':
		// Cursor forward N (default 1). fish uses this to skip over the prompt
		// before redrawing input. Advance the column, padding with spaces if
		// it moves past the current end of line.
		n := csiParam(params, 1)
		e.cursorForward(n)

	case 'D':
		// Cursor back N (default 1).
		n := csiParam(params, 1)
		e.col -= n
		if e.col < 0 {
			e.col = 0
		}

	case 'K':
		// Erase in line. param 0 (default): clear from cursor to end of line.
		// param 1: cursor to start. param 2: whole line. fish uses 0.
		switch csiParam(params, 0) {
		case 0:
			if e.col < len(e.cells) {
				e.cells = e.cells[:e.col]
			}
		case 1:
			for k := 0; k < e.col && k < len(e.cells); k++ {
				e.cells[k] = cell{r: []byte{' '}}
			}
		case 2:
			e.cells = e.cells[:0]
			e.col = 0
		}

	case 'G':
		// Cursor horizontal absolute (1-based). Not observed from fish here,
		// but cheap and correct to support.
		n := csiParam(params, 1)
		if n < 1 {
			n = 1
		}
		e.col = n - 1
		if e.col < 0 {
			e.col = 0
		}

	default:
		// Other CSI (e.g. bracketed-paste ?2004h): not a visible/cursor op we
		// model; discard so it never leaks into the view as text.
	}
}

// cursorForward advances the cursor by n columns, padding the current line with
// blank cells if it moves past the end.
func (e *lineEmulator) cursorForward(n int) {
	if n < 1 {
		n = 1
	}
	e.col += n
	for len(e.cells) < e.col {
		e.cells = append(e.cells, cell{r: []byte{' '}})
	}
}

// writeRune writes a printable rune at the cursor, overwriting any existing
// cell, then advances the cursor. Any pending attribute prefix is attached.
func (e *lineEmulator) writeRune(r []byte) {
	c := cell{r: append([]byte(nil), r...)}
	if len(e.pendingAttrs) > 0 {
		c.attrs = e.pendingAttrs
		e.pendingAttrs = nil
	}
	if e.col < len(e.cells) {
		e.cells[e.col] = c
	} else {
		for len(e.cells) < e.col {
			e.cells = append(e.cells, cell{r: []byte{' '}})
		}
		e.cells = append(e.cells, c)
	}
	e.col++
}

// maxCommittedLines bounds the scrollback the emulator retains. The full
// rendered buffer is reparsed by tview on every update, so this caps both
// memory and per-chunk render cost. Scrollback beyond this is dropped (v1
// limitation; the Console is for interactive use, not long-term log capture).
const maxCommittedLines = 5000

// lineFeed commits the current line and starts a fresh one. Any pending
// attributes carry to the new line (colour persists across lines, as on a real
// terminal).
func (e *lineEmulator) lineFeed() {
	e.committed = append(e.committed, e.renderCells(e.cells))
	if len(e.committed) > maxCommittedLines {
		// Drop the oldest lines, keeping the most recent maxCommittedLines.
		e.committed = append([]string(nil), e.committed[len(e.committed)-maxCommittedLines:]...)
	}
	e.cells = nil
	e.col = 0
}

// render reconstructs the full output: committed lines plus the live line.
func (e *lineEmulator) render() string {
	var b []byte
	for _, line := range e.committed {
		b = append(b, line...)
		b = append(b, '\n')
	}
	b = append(b, e.renderCells(e.cells)...)
	return string(b)
}

// renderCells reconstructs a single line's bytes from its cells.
func (e *lineEmulator) renderCells(cells []cell) string {
	var b []byte
	for _, c := range cells {
		b = append(b, c.attrs...)
		b = append(b, c.r...)
	}
	return string(b)
}

// csiParam parses the first numeric parameter of a CSI sequence, returning def
// when absent or unparsable.
func csiParam(params string, def int) int {
	if params == "" {
		return def
	}
	// Only the first parameter matters for the ops we handle.
	for i := 0; i < len(params); i++ {
		if params[i] == ';' {
			params = params[:i]
			break
		}
	}
	if params == "" {
		return def
	}
	n, err := strconv.Atoi(params)
	if err != nil {
		return def
	}
	return n
}

// isCSIFinal reports whether b terminates a CSI sequence (final byte range).
func isCSIFinal(b byte) bool {
	return b >= 0x40 && b <= 0x7e
}

// utf8Len returns the byte length of a UTF-8 sequence from its lead byte.
func utf8Len(lead byte) int {
	switch {
	case lead < 0x80:
		return 1
	case lead&0xE0 == 0xC0:
		return 2
	case lead&0xF0 == 0xE0:
		return 3
	case lead&0xF8 == 0xF0:
		return 4
	default:
		return 1 // invalid lead; treat as single byte
	}
}
