package controlcenter

// consoleFilter pre-processes raw PTY bytes before they reach tview's
// line-oriented ANSIWriter. tview's ANSI parser understands CSI (ESC[...)
// and OSC (ESC]...) sequences but silently drops the ESC of the
// charset-designation family (ESC ( x, ESC ) x, ESC * x, ESC + x) without
// consuming the trailing final byte, so the final byte (commonly "B" for
// US-ASCII or "0" for DEC graphics) leaks into the view as literal text
// (e.g. "BjasonB@Bubuntu-gpuB"). It also strips carriage returns, which
// the line-oriented view would otherwise render as a stray control glyph.
//
// The filter is stateful across calls so that escape sequences and CRLF
// pairs that straddle a read boundary (the PTY is read in fixed-size
// chunks) are handled correctly. A single consoleFilter belongs to one
// session's read loop, so no additional locking is required.
//
// Only CSI/OSC-style sequences relevant to coloring are passed through
// untouched; the charset-designation family is consumed and discarded.
type consoleFilter struct {
	// pending holds a partial, unresolved prefix carried over from the end
	// of the previous chunk. It is one of:
	//   - empty: no carry
	//   - [ESC]: a dangling escape whose next byte is not yet known
	//   - [ESC, intermediate]: a charset designator awaiting its final byte
	//   - ['\r']: a carriage return awaiting a possible following '\n'
	pending []byte
}

const escByte = 0x1b

// isCharsetIntermediate reports whether b is one of the charset-designation
// intermediate bytes that follow ESC: '(' G0, ')' G1, '*' G2, '+' G3.
func isCharsetIntermediate(b byte) bool {
	return b == '(' || b == ')' || b == '*' || b == '+'
}

// filter processes a chunk of raw PTY bytes, stripping charset-designation
// escape sequences and carriage returns, and returns the bytes safe to hand
// to tview's ANSIWriter. Any trailing partial sequence is retained in the
// filter and resolved on the next call.
func (f *consoleFilter) filter(input []byte) []byte {
	out := make([]byte, 0, len(input)+len(f.pending))

	// Resume from any carried-over prefix by prepending it to the work.
	work := input
	if len(f.pending) > 0 {
		work = append(append(make([]byte, 0, len(f.pending)+len(input)), f.pending...), input...)
		f.pending = nil
	}

	i := 0
	for i < len(work) {
		b := work[i]

		switch {
		case b == '\r':
			// Drop carriage returns. A following '\n' (the common CRLF
			// line ending) still produces the line break on its own. If
			// the '\r' is the final byte, carry it so we don't mistakenly
			// emit anything before knowing what follows.
			if i == len(work)-1 {
				f.pending = []byte{'\r'}
				return out
			}
			i++

		case b == escByte:
			// Need at least one more byte to classify the escape.
			if i+1 >= len(work) {
				f.pending = []byte{escByte}
				return out
			}
			next := work[i+1]
			if isCharsetIntermediate(next) {
				// Charset designation: ESC, intermediate, final byte.
				// Consume all three and emit nothing.
				if i+2 >= len(work) {
					// Final byte not yet available; carry ESC+intermediate.
					f.pending = []byte{escByte, next}
					return out
				}
				i += 3
			} else {
				// Any other escape (CSI '[', OSC ']', etc.) is left for
				// tview's ANSIWriter to interpret. Emit the ESC and let
				// normal copying continue with the next byte.
				out = append(out, b)
				i++
			}

		default:
			out = append(out, b)
			i++
		}
	}

	return out
}
