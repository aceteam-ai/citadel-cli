package deskstream

import "testing"

// TestFramer_PrependsSPSPPSOnKeyframe verifies the core wire-contract rule:
// when SPS+PPS appear only once at stream start, every later IDR keyframe still
// carries SPS+PPS in front of it.
func TestFramer_PrependsSPSPPSOnKeyframe(t *testing.T) {
	sps := nal(nalTypeSPS, 0xAA)
	pps := nal(nalTypePPS, 0xBB)
	idr1 := nal(nalTypeIDR, 0x11)
	pslice := nal(1, 0x22) // P-frame
	idr2 := nal(nalTypeIDR, 0x33)

	f := newNALFramer()

	// First access unit: SPS + PPS + IDR. The trailing pslice has no following
	// start code yet, so it is buffered until the next NAL terminates it (this
	// mirrors how ffmpeg stdout streams). So this Push emits exactly the keyframe
	// payload; the P-frame flushes on the next Push.
	out := f.Push(concat(sps, pps, idr1, pslice))
	if len(out) != 1 {
		t.Fatalf("got %d payloads, want 1 (keyframe; P-frame buffered): %v", len(out), out)
	}
	if !payloadHasKeyframe(out[0]) {
		t.Fatal("first payload should contain the keyframe")
	}
	// The keyframe payload must begin with SPS then PPS.
	units := scanNALUnits(out[0])
	if len(units) < 3 || units[0].Type != nalTypeSPS || units[1].Type != nalTypePPS || units[2].Type != nalTypeIDR {
		t.Fatalf("keyframe payload not SPS,PPS,IDR: %v", typesOf(units))
	}

	// Second keyframe arrives WITHOUT inline SPS/PPS. The framer must prepend
	// the cached ones. (The previously buffered P-frame flushes first, then the
	// keyframe payload; find the keyframe payload among the outputs.)
	out2 := f.Push(concat(idr2, nal(1, 0x44)))
	var kf []byte
	for _, p := range out2 {
		if payloadHasKeyframe(p) {
			kf = p
		}
	}
	if kf == nil {
		t.Fatalf("expected a keyframe payload, got %d payloads", len(out2))
	}
	kunits := scanNALUnits(kf)
	if len(kunits) < 3 || kunits[0].Type != nalTypeSPS || kunits[1].Type != nalTypePPS || kunits[2].Type != nalTypeIDR {
		t.Fatalf("second keyframe missing prepended SPS/PPS: %v", typesOf(kunits))
	}
	// And the prepended SPS/PPS must be byte-identical to the originals.
	if !equalNAL(kunits[0].Data, sps) || !equalNAL(kunits[1].Data, pps) {
		t.Error("prepended SPS/PPS bytes differ from cached originals")
	}
}

// TestFramer_DoesNotDuplicateInlineParamSets ensures we do not prepend cached
// SPS/PPS when the encoder already emitted them inline with the IDR.
func TestFramer_DoesNotDuplicateInlineParamSets(t *testing.T) {
	sps := nal(nalTypeSPS, 0xAA)
	pps := nal(nalTypePPS, 0xBB)
	idr := nal(nalTypeIDR, 0x11)

	f := newNALFramer()
	out := f.Push(concat(sps, pps, idr, nal(1, 0x22)))
	if len(out) < 1 {
		t.Fatal("expected a keyframe payload")
	}
	units := scanNALUnits(out[0])
	spsCount := 0
	for _, u := range units {
		if u.Type == nalTypeSPS {
			spsCount++
		}
	}
	if spsCount != 1 {
		t.Errorf("SPS appears %d times, want exactly 1 (no duplication)", spsCount)
	}
}

// TestFramer_PFrameNotPrefixed ensures non-keyframe payloads are NOT prefixed
// with parameter sets.
func TestFramer_PFrameNotPrefixed(t *testing.T) {
	sps := nal(nalTypeSPS, 0xAA)
	pps := nal(nalTypePPS, 0xBB)
	idr := nal(nalTypeIDR, 0x11)
	p1 := nal(1, 0x22)
	p2 := nal(1, 0x33)

	f := newNALFramer()
	// prime SPS/PPS via a keyframe.
	f.Push(concat(sps, pps, idr, p1, p2))
	// A standalone P-frame stream.
	out := f.Push(concat(nal(1, 0x44), nal(1, 0x55)))
	for _, payload := range out {
		if payloadHasKeyframe(payload) {
			t.Fatal("P-frame payload unexpectedly flagged as keyframe")
		}
		for _, u := range scanNALUnits(payload) {
			if u.IsParameterSet() {
				t.Error("P-frame payload should not carry SPS/PPS")
			}
		}
	}
}

func TestFramer_HasParameterSets(t *testing.T) {
	f := newNALFramer()
	if f.HasParameterSets() {
		t.Error("fresh framer should not have parameter sets")
	}
	f.Push(concat(nal(nalTypeSPS, 0x01), nal(nalTypePPS, 0x02), nal(nalTypeIDR, 0x03), nal(1, 0x04)))
	if !f.HasParameterSets() {
		t.Error("framer should have parameter sets after a keyframe with SPS/PPS")
	}
}

func typesOf(units []NALUnit) []int {
	out := make([]int, len(units))
	for i, u := range units {
		out[i] = u.Type
	}
	return out
}
