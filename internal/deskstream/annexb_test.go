package deskstream

import "testing"

// nal builds a NAL unit with a 4-byte start code and the given type byte.
func nal(typ byte, payload ...byte) []byte {
	b := []byte{0, 0, 0, 1, typ & 0x1f}
	return append(b, payload...)
}

// nal3 builds a NAL unit with a 3-byte start code.
func nal3(typ byte, payload ...byte) []byte {
	b := []byte{0, 0, 1, typ & 0x1f}
	return append(b, payload...)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestStartCodeLen(t *testing.T) {
	if got := startCodeLen([]byte{0, 0, 0, 1, 0x67}); got != 4 {
		t.Errorf("4-byte start code: got %d, want 4", got)
	}
	if got := startCodeLen([]byte{0, 0, 1, 0x67}); got != 3 {
		t.Errorf("3-byte start code: got %d, want 3", got)
	}
	if got := startCodeLen([]byte{1, 2, 3, 4}); got != 0 {
		t.Errorf("no start code: got %d, want 0", got)
	}
}

func TestNalType(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		{"sps", nal(nalTypeSPS), nalTypeSPS},
		{"pps", nal(nalTypePPS), nalTypePPS},
		{"idr", nal(nalTypeIDR), nalTypeIDR},
		{"sps-3byte", nal3(nalTypeSPS), nalTypeSPS},
		{"no-start-code", []byte{0x67}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nalType(c.in); got != c.want {
				t.Errorf("nalType = %d, want %d", got, c.want)
			}
		})
	}
}

func TestSplitNALUnits_CompleteAndPartial(t *testing.T) {
	sps := nal(nalTypeSPS, 0xAA)
	pps := nal(nalTypePPS, 0xBB)
	idr := nal(nalTypeIDR, 0xCC, 0xDD)
	stream := concat(sps, pps, idr)

	// A trailing partial NAL (start code present but not terminated) should be
	// carried over as remainder, not emitted.
	partial := []byte{0, 0, 0, 1, nalTypeIDR, 0x01}
	full := concat(stream, partial)

	units, remainder := splitNALUnits(full)
	if len(units) != 3 {
		t.Fatalf("got %d complete units, want 3", len(units))
	}
	if units[0].Type != nalTypeSPS || units[1].Type != nalTypePPS || units[2].Type != nalTypeIDR {
		t.Errorf("unexpected types: %d %d %d", units[0].Type, units[1].Type, units[2].Type)
	}
	if remainder != len(stream) {
		t.Errorf("remainder = %d, want %d (start of partial)", remainder, len(stream))
	}
	if !equalNAL(full[remainder:], partial) {
		t.Errorf("remainder bytes = %v, want %v", full[remainder:], partial)
	}
}

func TestSplitNALUnits_NoStartCode(t *testing.T) {
	units, remainder := splitNALUnits([]byte{1, 2, 3, 4, 5})
	if units != nil {
		t.Errorf("expected no units, got %d", len(units))
	}
	if remainder != 0 {
		t.Errorf("remainder = %d, want 0", remainder)
	}
}

func TestNALUnitClassifiers(t *testing.T) {
	idr := NALUnit{Type: nalTypeIDR}
	if !idr.IsKeyframe() {
		t.Error("IDR should be a keyframe")
	}
	if idr.IsParameterSet() {
		t.Error("IDR is not a parameter set")
	}
	sps := NALUnit{Type: nalTypeSPS}
	if !sps.IsParameterSet() {
		t.Error("SPS should be a parameter set")
	}
	if sps.IsKeyframe() {
		t.Error("SPS is not a keyframe")
	}
}

func TestPayloadHasKeyframe(t *testing.T) {
	withIDR := concat(nal(nalTypeSPS), nal(nalTypePPS), nal(nalTypeIDR))
	if !payloadHasKeyframe(withIDR) {
		t.Error("payload with IDR should report keyframe")
	}
	withoutIDR := nal(1, 0x01) // a non-IDR slice
	if payloadHasKeyframe(withoutIDR) {
		t.Error("payload without IDR should not report keyframe")
	}
}
