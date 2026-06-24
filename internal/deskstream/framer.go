package deskstream

import "bytes"

// nalFramer consumes a streaming Annex-B byte stream (e.g. ffmpeg stdout) and
// produces WebSocket BINARY payloads ready to send to clients, applying the
// wire contract's keyframe rule:
//
//	SPS+PPS are prepended to every IDR keyframe.
//
// The framer caches the most recently seen SPS and PPS so that even encoders
// which only emit parameter sets once (at stream start) still get them in front
// of every keyframe. This makes any single keyframe payload independently
// decodable by a client that joined the broadcast mid-GOP.
//
// nalFramer is NOT safe for concurrent use; drive it from a single goroutine.
type nalFramer struct {
	sps []byte // latest SPS NAL unit (with start code), or nil
	pps []byte // latest PPS NAL unit (with start code), or nil

	buf []byte // carry-over bytes for a partial trailing NAL unit
}

// newNALFramer creates an empty framer.
func newNALFramer() *nalFramer { return &nalFramer{} }

// Push appends a chunk of Annex-B bytes (as read from ffmpeg stdout) and
// returns zero or more complete WebSocket BINARY payloads. Each returned
// payload is a self-contained sequence of one or more NAL units:
//
//   - A non-keyframe access unit is forwarded as-is.
//   - A keyframe (IDR) access unit is prefixed with the cached SPS+PPS (unless
//     the encoder already emitted them inline, in which case they are not
//     duplicated).
//
// SPS/PPS NAL units that arrive on their own (not adjacent to a slice) update
// the cache and are not emitted separately; they ride in front of the next
// keyframe instead.
func (f *nalFramer) Push(chunk []byte) [][]byte {
	f.buf = append(f.buf, chunk...)
	units, remainder := splitNALUnits(f.buf)
	// Retain the unconsumed tail (a partial NAL) for the next Push. The NAL
	// slices in units alias f.buf, so we must NOT compact f.buf in place here
	// (that would overwrite bytes the pending units still point at). Snapshot
	// the tail into a fresh backing array and replace f.buf with it.
	tail := append([]byte(nil), f.buf[remainder:]...)
	f.buf = tail

	var out [][]byte
	// pending accumulates the NAL units of the access unit being assembled, so
	// that SPS/PPS that immediately precede an IDR are kept together with it.
	var pending [][]byte
	var pendingHasKeyframe bool
	var pendingHasSPS, pendingHasPPS bool

	flush := func() {
		if len(pending) == 0 {
			return
		}
		var payload []byte
		if pendingHasKeyframe {
			// Prepend cached SPS/PPS unless already present inline.
			if !pendingHasSPS && f.sps != nil {
				payload = append(payload, f.sps...)
			}
			if !pendingHasPPS && f.pps != nil {
				payload = append(payload, f.pps...)
			}
		}
		for _, u := range pending {
			payload = append(payload, u...)
		}
		out = append(out, payload)
		pending = pending[:0]
		pendingHasKeyframe = false
		pendingHasSPS = false
		pendingHasPPS = false
	}

	for _, u := range units {
		switch {
		case u.Type == nalTypeSPS:
			f.sps = append([]byte(nil), u.Data...)
			pending = append(pending, u.Data)
			pendingHasSPS = true
		case u.Type == nalTypePPS:
			f.pps = append([]byte(nil), u.Data...)
			pending = append(pending, u.Data)
			pendingHasPPS = true
		case u.Type == nalTypeIDR:
			pending = append(pending, u.Data)
			pendingHasKeyframe = true
			flush()
		default:
			// A non-IDR slice (P-frame) or other NAL ends the access unit.
			pending = append(pending, u.Data)
			flush()
		}
	}
	// Leave any trailing SPS/PPS-only pending bytes cached for the next keyframe;
	// do not emit a parameter-set-only payload.
	if pendingHasKeyframe {
		flush()
	}
	return out
}

// HasParameterSets reports whether both an SPS and a PPS have been observed,
// which is the point at which keyframes become independently decodable.
func (f *nalFramer) HasParameterSets() bool { return f.sps != nil && f.pps != nil }

// payloadHasKeyframe reports whether a marshaled BINARY payload contains an IDR
// NAL unit. Exposed for tests and for connection bookkeeping.
func payloadHasKeyframe(payload []byte) bool {
	for _, u := range scanNALUnits(payload) {
		if u.Type == nalTypeIDR {
			return true
		}
	}
	return false
}

// scanNALUnits splits a COMPLETE Annex-B payload (one already framed for the
// wire, so fully terminated) into its NAL units. Unlike splitNALUnits it treats
// the final NAL as complete.
func scanNALUnits(b []byte) []NALUnit {
	first, _ := findStartCode(b, 0)
	if first < 0 {
		return nil
	}
	var units []NALUnit
	pos := first
	for {
		next, _ := findStartCode(b, pos+3)
		if next < 0 {
			nal := b[pos:]
			units = append(units, NALUnit{Data: nal, Type: nalType(nal)})
			return units
		}
		nal := b[pos:next]
		units = append(units, NALUnit{Data: nal, Type: nalType(nal)})
		pos = next
	}
}

// equalNAL reports whether two NAL unit payloads are byte-identical. Used by
// tests to assert SPS/PPS prepending.
func equalNAL(a, b []byte) bool { return bytes.Equal(a, b) }
