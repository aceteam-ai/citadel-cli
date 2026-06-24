package controlcenter

// deviceAttrResponder answers terminal "Device Attributes" (DA) queries that a
// shell emits at startup. The Console pane renders PTY output through tview's
// line-oriented ANSIWriter rather than a full terminal emulator, so nothing
// ever replies to these queries. Shells that probe terminal capabilities then
// block waiting for a response — fish, for example, waits ~2s and prints:
//
//	"fish could not read response to Primary Device Attribute query after
//	 waiting for 2 seconds ... This fish process will no longer wait for
//	 outstanding queries, which disables some optional features."
//
// The fix is to detect the query in the PTY output stream and write a canned
// reply back to the PTY (the shell's stdin), exactly as a real terminal would.
//
// Queries handled (both are CSI sequences terminated by the final byte 'c',
// which no display sequence uses, so matching is unambiguous):
//   - Primary DA   (DA1): CSI c   / CSI 0 c   -> reply CSI ? 1 ; 2 c
//   - Secondary DA (DA2): CSI > ... c          -> reply CSI > 0 ; 0 ; 0 c
//
// The Primary reply is deliberately minimal — a VT100 with the Advanced Video
// Option. Advertising more would invite the shell to enable query-driven
// features (cursor reports, synchronized output) that the line-oriented
// renderer cannot service. The point is only to satisfy "did the terminal
// answer", not to claim capabilities we do not have.
//
// The responder is stateful across reads so a query split over a PTY read
// boundary is still recognised. One responder belongs to a single session's
// read loop, so no locking is required. It never modifies the display bytes;
// tview's ANSIWriter already consumes the CSI query without rendering it.
type deviceAttrResponder struct {
	sawESC bool   // saw ESC, awaiting '['
	inCSI  bool   // inside a CSI sequence, collecting parameter bytes
	params []byte // parameter/intermediate bytes accumulated after "ESC["
}

// Replies advertise a basic terminal. See the type doc for why minimal.
var (
	primaryDAResponse   = []byte("\x1b[?1;2c")
	secondaryDAResponse = []byte("\x1b[>0;0;0c")
)

// maxCSIParamBytes bounds parameter accumulation so malformed/never-terminated
// CSI input cannot grow params without limit.
const maxCSIParamBytes = 32

// scan consumes a chunk of raw PTY output and returns the bytes (if any) to
// write back to the PTY in reply to Device Attributes queries found in it.
// Multiple queries in one chunk yield concatenated replies. It is purely a
// detector: the input is never modified.
func (r *deviceAttrResponder) scan(input []byte) []byte {
	var resp []byte
	for _, b := range input {
		switch {
		case r.inCSI:
			if b >= 0x40 && b <= 0x7e {
				// Final byte: the CSI sequence ends here.
				if b == 'c' {
					resp = append(resp, r.replyForParams()...)
				}
				r.inCSI = false
				r.params = r.params[:0]
			} else {
				r.params = append(r.params, b)
				if len(r.params) > maxCSIParamBytes {
					// Give up on a runaway sequence; resync on the next ESC.
					r.inCSI = false
					r.params = r.params[:0]
				}
			}
		case r.sawESC:
			r.sawESC = false
			switch b {
			case '[':
				r.inCSI = true
			case escByte:
				r.sawESC = true // ESC ESC: stay armed for the next byte
			}
		default:
			if b == escByte {
				r.sawESC = true
			}
		}
	}
	return resp
}

// replyForParams selects the reply for a CSI ... 'c' query based on its
// leading parameter byte: '>' is Secondary DA, '=' is Tertiary DA (ignored),
// everything else (empty or numeric, i.e. Primary DA) gets the Primary reply.
func (r *deviceAttrResponder) replyForParams() []byte {
	if len(r.params) > 0 {
		switch r.params[0] {
		case '>':
			return secondaryDAResponse
		case '=':
			return nil // Tertiary DA: no meaningful reply from this renderer.
		}
	}
	return primaryDAResponse
}
