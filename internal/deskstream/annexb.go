// Package deskstream serves the node desktop as an H.264 video stream over a
// binary WebSocket on the node mesh listener (citadel-cli#338). It mirrors the
// VNC server's exposure model: it listens on localhost plus any tsnet VPN
// listeners attached via AddListener, with no application-layer auth (the tsnet
// mesh is the trust boundary, exactly like internal/desktop's VNCServer).
//
// Input (mouse/keyboard) is unchanged and stays on the existing action path;
// this package is VIDEO-ONLY.
package deskstream

// H.264 in Annex-B format is a sequence of NAL (Network Abstraction Layer)
// units, each prefixed by a start code of either 3 bytes (00 00 01) or 4 bytes
// (00 00 00 01). The low 5 bits of the byte after the start code give the NAL
// unit type. We care about three types when reconstructing keyframes for a
// broadcast stream:
//
//	7 = SPS (Sequence Parameter Set)
//	8 = PPS (Picture Parameter Set)
//	5 = IDR slice (an instantaneous decoder refresh keyframe)
//
// A decoder that joins a broadcast mid-GOP cannot decode P-frames (which
// reference prior encoder state), so the wire contract requires SPS+PPS to be
// prepended on every IDR. We parse ffmpeg's Annex-B output ourselves (rather
// than rely on encoder-specific header-repeat flags, which differ across
// vaapi/nvenc/libx264) so the behavior is identical regardless of encoder.
const (
	nalTypeIDR = 5
	nalTypeSPS = 7
	nalTypePPS = 8
)

// NALUnit is a single Annex-B NAL unit including its leading start code.
type NALUnit struct {
	// Data is the full NAL unit bytes, INCLUDING the start code prefix.
	Data []byte
	// Type is the NAL unit type (low 5 bits of the byte after the start code).
	Type int
}

// IsKeyframe reports whether this NAL unit is an IDR slice (a keyframe).
func (n NALUnit) IsKeyframe() bool { return n.Type == nalTypeIDR }

// IsParameterSet reports whether this NAL unit is an SPS or PPS.
func (n NALUnit) IsParameterSet() bool { return n.Type == nalTypeSPS || n.Type == nalTypePPS }

// startCodeLen returns the length of the Annex-B start code at the beginning of
// b (4 for 00 00 00 01, 3 for 00 00 01), or 0 if b does not begin with a start
// code.
func startCodeLen(b []byte) int {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return 4
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return 3
	}
	return 0
}

// findStartCode returns the index of the next Annex-B start code at or after
// offset start, plus the length of that start code. It returns (-1, 0) if none
// is found. A 4-byte start code is reported as a 3-byte code at index+1 only
// when the extra leading zero is part of the previous NAL's trailing bytes;
// callers normalize by checking the byte before the match. To keep the parser
// simple and unambiguous we always match the minimal 00 00 01 and let the
// caller fold a preceding zero into the start code via startCodeLen on the
// emitted slice.
func findStartCode(b []byte, start int) (idx int, scLen int) {
	for i := start; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			// Prefer the 4-byte form when a leading zero precedes the 00 00 01.
			if i > 0 && b[i-1] == 0 {
				return i - 1, 4
			}
			return i, 3
		}
	}
	return -1, 0
}

// nalType extracts the NAL unit type from a NAL unit that begins with a start
// code. Returns -1 if the slice is too short or has no start code.
func nalType(nal []byte) int {
	sc := startCodeLen(nal)
	if sc == 0 || len(nal) <= sc {
		return -1
	}
	return int(nal[sc] & 0x1f)
}

// splitNALUnits splits an Annex-B byte slice into complete NAL units, each
// retaining its leading start code. A trailing partial NAL (one not terminated
// by a following start code) is NOT returned; instead its byte offset within b
// is returned as remainder so the caller can carry it into the next read. This
// makes the function safe to drive from a streaming ffmpeg stdout pipe.
//
// remainder is the index in b where the unconsumed tail begins (len(b) when all
// bytes were consumed into complete NAL units). Callers retain b[remainder:].
func splitNALUnits(b []byte) (units []NALUnit, remainder int) {
	// Locate the first start code; anything before it is not a NAL unit.
	first, _ := findStartCode(b, 0)
	if first < 0 {
		return nil, 0
	}

	pos := first
	for {
		// Find the start of the NEXT NAL unit, which terminates the current one.
		next, _ := findStartCode(b, pos+3)
		if next < 0 {
			// No following start code: the current NAL is incomplete (its full
			// extent is unknown until more bytes arrive). Carry it over.
			return units, pos
		}
		nal := b[pos:next]
		units = append(units, NALUnit{Data: nal, Type: nalType(nal)})
		pos = next
	}
}
