package jobs

import (
	"encoding/json"
	"reflect"
	"testing"
)

func hasReason(g TranscriptionGuard, want string) bool {
	for _, r := range g.Reasons {
		if r == want {
			return true
		}
	}
	return false
}

// TestEvaluateGuard_FabricatedFillerFlaggedFromSignals is the core incident:
// a single confident-looking "Thank you very much." segment that the model's
// OWN signals betray as no-speech (high no_speech_prob) and low-confidence
// (low avg_logprob). The guard must flag both rather than pass the filler as
// content.
func TestEvaluateGuard_FabricatedFillerFlaggedFromSignals(t *testing.T) {
	segs := []guardSegment{
		{Text: "Thank you very much.", NoSpeechProb: 0.92, AvgLogprob: -1.8, CompressionRatio: 0.9, signalsPresent: true},
	}
	g := evaluateTranscriptionGuard(segs, "Thank you very much.", 0.98, false, 0)
	if !g.NoSpeech {
		t.Errorf("expected NoSpeech=true for high no_speech_prob, got %+v", g)
	}
	if !g.LowConfidence {
		t.Errorf("expected LowConfidence=true, got %+v", g)
	}
	if !g.SignalsAvailable {
		t.Error("expected SignalsAvailable=true")
	}
	if !hasReason(g, guardReasonHighNoSpeech) || !hasReason(g, guardReasonLowLogprob) {
		t.Errorf("missing expected reasons, got %v", g.Reasons)
	}
}

// TestEvaluateGuard_EmptyTranscriptIsNoSpeech: an empty transcript is
// unambiguous no-speech and needs no signals at all.
func TestEvaluateGuard_EmptyTranscriptIsNoSpeech(t *testing.T) {
	g := evaluateTranscriptionGuard(nil, "   ", 0, false, 0)
	if !g.NoSpeech || !g.LowConfidence {
		t.Errorf("expected empty transcript flagged, got %+v", g)
	}
	if !hasReason(g, guardReasonEmptyTranscript) {
		t.Errorf("expected empty_transcript reason, got %v", g.Reasons)
	}
	if g.SignalsAvailable {
		t.Error("empty transcript should report SignalsAvailable=false")
	}
}

// TestEvaluateGuard_CleanSpeechNotFlagged: normal speech with healthy signals
// must produce a clean verdict (this is the common case; the guard must not
// cry wolf on it).
func TestEvaluateGuard_CleanSpeechNotFlagged(t *testing.T) {
	segs := []guardSegment{
		{Text: "Hello, thanks for joining the call today.", NoSpeechProb: 0.02, AvgLogprob: -0.2, CompressionRatio: 1.3, signalsPresent: true},
		{Text: "Let's start with the quarterly numbers.", NoSpeechProb: 0.03, AvgLogprob: -0.25, CompressionRatio: 1.4, signalsPresent: true},
	}
	g := evaluateTranscriptionGuard(segs, "Hello, thanks for joining the call today. Let's start with the quarterly numbers.", 0.99, false, 0)
	if g.NoSpeech || g.LowConfidence || g.Repetition {
		t.Errorf("clean speech should not be flagged, got %+v", g)
	}
	if len(g.Reasons) != 0 {
		t.Errorf("expected no reasons, got %v", g.Reasons)
	}
}

// TestEvaluateGuard_RepetitionLoopWithoutSignals proves the signal-INDEPENDENT
// path: a dominant-phrase loop (the "It's a pleasure to meet you." case) is
// caught even when the sidecar emitted no per-segment probabilities (an old
// image), so the fix partially works pre-rebuild.
func TestEvaluateGuard_RepetitionLoopWithoutSignals(t *testing.T) {
	segs := make([]guardSegment, 6)
	for i := range segs {
		segs[i] = guardSegment{Text: "It's a pleasure to meet you.", signalsPresent: false}
	}
	g := evaluateTranscriptionGuard(segs, "", 0, false, 0)
	if !g.Repetition || !g.LowConfidence {
		t.Errorf("expected repetition loop flagged, got %+v", g)
	}
	if g.SignalsAvailable {
		t.Error("expected SignalsAvailable=false with no per-segment signals")
	}
	if !hasReason(g, guardReasonRepetitionLoop) {
		t.Errorf("expected repetition_loop reason, got %v", g.Reasons)
	}
}

// TestEvaluateGuard_LowDistinctRatioLoop covers the Welsh-gibberish shape: many
// segments cycling only a couple of distinct phrases.
func TestEvaluateGuard_LowDistinctRatioLoop(t *testing.T) {
	segs := []guardSegment{
		{Text: "Diolch yn fawr", signalsPresent: false},
		{Text: "Diolch yn fawr iawn", signalsPresent: false},
		{Text: "Diolch yn fawr", signalsPresent: false},
		{Text: "Diolch yn fawr iawn", signalsPresent: false},
		{Text: "Diolch yn fawr", signalsPresent: false},
		{Text: "Diolch yn fawr iawn", signalsPresent: false},
	}
	g := evaluateTranscriptionGuard(segs, "", 0, false, 0)
	if !g.Repetition {
		t.Errorf("expected low distinct-ratio loop flagged, got %+v", g)
	}
}

// TestEvaluateGuard_UncertainLanguageOnlyWithoutHint: a low language-detect
// probability with NO caller hint flags uncertain_language; the SAME low
// probability WITH a hint does not (the caller already told us the language).
func TestEvaluateGuard_UncertainLanguageOnlyWithoutHint(t *testing.T) {
	segs := []guardSegment{
		{Text: "some plausible words here", NoSpeechProb: 0.1, AvgLogprob: -0.5, CompressionRatio: 1.2, signalsPresent: true},
	}
	noHint := evaluateTranscriptionGuard(segs, "some plausible words here", 0.30, false, 0)
	if !noHint.LowConfidence || !hasReason(noHint, guardReasonUncertainLang) {
		t.Errorf("expected uncertain_language low-confidence without a hint, got %+v", noHint)
	}

	withHint := evaluateTranscriptionGuard(segs, "some plausible words here", 0.30, true, 0)
	if hasReason(withHint, guardReasonUncertainLang) {
		t.Errorf("uncertain_language must NOT fire when the caller supplied a language hint, got %v", withHint.Reasons)
	}
}

// TestEvaluateGuard_AbsentSignalsNeverAssertNoSpeech: the fail-open direction —
// zero-valued but ABSENT signals (an old sidecar) must never be read as a
// confident no-speech. avg_logprob==0 would look like perfect confidence and
// no_speech_prob==0 like definite speech; the guard must not manufacture EITHER
// verdict from absence.
func TestEvaluateGuard_AbsentSignalsNeverAssertNoSpeech(t *testing.T) {
	segs := []guardSegment{
		{Text: "a normal sentence of speech", signalsPresent: false},
	}
	g := evaluateTranscriptionGuard(segs, "a normal sentence of speech", 0, false, 0)
	if g.NoSpeech {
		t.Errorf("must not assert NoSpeech from absent signals, got %+v", g)
	}
	if g.SignalsAvailable {
		t.Error("SignalsAvailable must be false")
	}
	if g.LowConfidence {
		t.Errorf("must not assert LowConfidence from absent signals on non-repetitive text, got %+v", g)
	}
}

// TestEvaluateGuard_HighCompressionSingleSegment: a single segment with a high
// compression ratio (the within-segment repetition tell) flags repetition when
// signals are available.
func TestEvaluateGuard_HighCompressionSingleSegment(t *testing.T) {
	segs := []guardSegment{
		{Text: "ha ha ha ha ha ha ha ha ha ha ha", NoSpeechProb: 0.1, AvgLogprob: -0.4, CompressionRatio: 3.1, signalsPresent: true},
	}
	g := evaluateTranscriptionGuard(segs, "ha ha ha ha ha ha ha ha ha ha ha", 0.9, false, 0)
	if !g.Repetition || !g.LowConfidence {
		t.Errorf("expected high-compression repetition flagged, got %+v", g)
	}
	if !hasReason(g, guardReasonHighCompression) {
		t.Errorf("expected high_compression_ratio reason, got %v", g.Reasons)
	}
}

// TestEvaluateGuard_LowSpeechCoverageFlagged is the incident's own signature:
// one short segment standing in for a long recording. Even though this lone
// segment's probabilities sit INSIDE faster-whisper's thresholds (it survived
// the engine's skip rule and reached the output), the speech-coverage floor
// catches it.
func TestEvaluateGuard_LowSpeechCoverageFlagged(t *testing.T) {
	segs := []guardSegment{
		{Text: "Thank you very much.", Start: 0.0, End: 2.0, NoSpeechProb: 0.3, AvgLogprob: -0.4, CompressionRatio: 0.8, signalsPresent: true},
	}
	// 2 seconds of speech across a 38-minute (2280s) recording -> ~0.09% coverage.
	g := evaluateTranscriptionGuard(segs, "Thank you very much.", 0.95, true, 2280)
	if !g.NoSpeech || !g.LowConfidence {
		t.Errorf("expected low-coverage transcript flagged as no-speech, got %+v", g)
	}
	if !hasReason(g, guardReasonLowCoverage) {
		t.Errorf("expected low_speech_coverage reason, got %v", g.Reasons)
	}

	// A short clip that is mostly speech must NOT be flagged (coverage high).
	dense := []guardSegment{
		{Text: "quick note before we wrap up", Start: 0.0, End: 25.0, NoSpeechProb: 0.05, AvgLogprob: -0.3, CompressionRatio: 1.2, signalsPresent: true},
	}
	gd := evaluateTranscriptionGuard(dense, "quick note before we wrap up", 0.98, true, 30)
	if hasReason(gd, guardReasonLowCoverage) {
		t.Errorf("dense short clip must not trip low_speech_coverage, got %+v", gd)
	}

	// Unknown duration (0) must fail open: coverage is never evaluated.
	gu := evaluateTranscriptionGuard(segs, "Thank you very much.", 0.95, true, 0)
	if hasReason(gu, guardReasonLowCoverage) {
		t.Errorf("unknown duration must not trip low_speech_coverage, got %+v", gu)
	}
	if gu.SpeechCoverage != -1 {
		t.Errorf("unknown duration should leave SpeechCoverage=-1, got %v", gu.SpeechCoverage)
	}
}

// TestEvaluateGuard_BackchannelNotFlaggedAsRepetition guards the #4 false
// positive: short backchannel exchanges must never register as a hallucination
// loop, even with several identical single-word segments. The repeated content
// must be a real phrase (>=3 words) for the loop rules to fire.
func TestEvaluateGuard_BackchannelNotFlaggedAsRepetition(t *testing.T) {
	// The advisor's example: three segments, 2/3 dominant — must NOT flag (below
	// the segment floor).
	short := []guardSegment{
		{Text: "Yeah.", signalsPresent: false},
		{Text: "Yeah.", signalsPresent: false},
		{Text: "Okay, let's go.", signalsPresent: false},
	}
	if g := evaluateTranscriptionGuard(short, "", 0, false, 0); g.Repetition {
		t.Errorf("short backchannel exchange must not flag repetition, got %+v", g)
	}

	// Even five identical single-word backchannels must not flag: the repeated
	// phrase is one word, below guardMinRepeatedPhraseWords.
	many := make([]guardSegment, 5)
	for i := range many {
		many[i] = guardSegment{Text: "Yeah.", signalsPresent: false}
	}
	if g := evaluateTranscriptionGuard(many, "", 0, false, 0); g.Repetition {
		t.Errorf("repeated single-word backchannel must not flag repetition, got %+v", g)
	}
}

// TestAttachGuard_AdditivePreservesOtherFields proves the augmentation is
// provably additive: every original top-level field round-trips byte-for-byte
// (decoded as raw messages), only `transcription_guard` is added.
func TestAttachGuard_AdditivePreservesOtherFields(t *testing.T) {
	raw := []byte(`{"text":"hello world","language":"en","language_probability":0.98,"segments":[{"start":0.0,"end":1.5,"text":"hello world","no_speech_prob":0.01,"avg_logprob":-0.2,"compression_ratio":1.1}],"diarization":"none"}`)
	out := attachTranscriptionGuard(raw, false)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if _, ok := top["transcription_guard"]; !ok {
		t.Fatal("transcription_guard not attached")
	}

	// Compare every original field against a raw re-parse of the input.
	var orig map[string]json.RawMessage
	if err := json.Unmarshal(raw, &orig); err != nil {
		t.Fatalf("input not JSON: %v", err)
	}
	for k, v := range orig {
		got, ok := top[k]
		if !ok {
			t.Errorf("original field %q dropped", k)
			continue
		}
		if !reflect.DeepEqual([]byte(v), []byte(got)) {
			t.Errorf("field %q changed: was %s, now %s", k, v, got)
		}
	}

	// The clean case must not be flagged.
	var guard TranscriptionGuard
	if err := json.Unmarshal(top["transcription_guard"], &guard); err != nil {
		t.Fatalf("guard not decodable: %v", err)
	}
	if guard.NoSpeech || guard.LowConfidence {
		t.Errorf("clean transcript flagged: %+v", guard)
	}
	if !guard.SignalsAvailable {
		t.Error("expected SignalsAvailable=true (segment carried signals)")
	}
}

// TestAttachGuard_OldSidecarSegmentsWithoutSignals: an old sidecar emits
// segments with no probability fields; the guard still catches a repetition
// loop via text and reports signals_available=false.
func TestAttachGuard_OldSidecarSegmentsWithoutSignals(t *testing.T) {
	raw := []byte(`{"text":"Thank you very much. Thank you very much. Thank you very much. Thank you very much.","segments":[{"start":0,"end":2,"text":"Thank you very much."},{"start":2,"end":4,"text":"Thank you very much."},{"start":4,"end":6,"text":"Thank you very much."},{"start":6,"end":8,"text":"Thank you very much."}]}`)
	out := attachTranscriptionGuard(raw, false)
	var top map[string]json.RawMessage
	_ = json.Unmarshal(out, &top)
	var guard TranscriptionGuard
	if err := json.Unmarshal(top["transcription_guard"], &guard); err != nil {
		t.Fatalf("guard not decodable: %v", err)
	}
	if guard.SignalsAvailable {
		t.Error("old sidecar segments should report SignalsAvailable=false")
	}
	if !guard.Repetition || !guard.LowConfidence {
		t.Errorf("expected repetition flagged from text alone, got %+v", guard)
	}
}

// TestAttachGuard_InvalidJSONReturnedVerbatim: a non-JSON or non-object body is
// relayed unchanged (the pre-fix behavior on any parse failure).
func TestAttachGuard_InvalidJSONReturnedVerbatim(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`not json at all`),
		[]byte(`[1,2,3]`),
		[]byte(`"a scalar string"`),
	} {
		out := attachTranscriptionGuard(raw, false)
		if !reflect.DeepEqual(out, raw) {
			t.Errorf("non-object body should be returned verbatim; in=%s out=%s", raw, out)
		}
	}
}

// TestAttachGuard_DoesNotClobberExistingField: if the sidecar ever emits its
// own transcription_guard, the Go side leaves the whole body untouched.
func TestAttachGuard_DoesNotClobberExistingField(t *testing.T) {
	raw := []byte(`{"text":"hi","segments":[],"transcription_guard":{"no_speech":false}}`)
	out := attachTranscriptionGuard(raw, false)
	if !reflect.DeepEqual(out, raw) {
		t.Errorf("existing transcription_guard must not be clobbered; in=%s out=%s", raw, out)
	}
}

// TestParseGuardSegments_SignalPresenceDetection pins the per-segment
// present/absent detection that keeps absent signals from being read as
// confident zeros.
func TestParseGuardSegments_SignalPresenceDetection(t *testing.T) {
	with := parseGuardSegments(json.RawMessage(`[{"text":"a","no_speech_prob":0.5,"avg_logprob":-0.3,"compression_ratio":1.2}]`))
	if len(with) != 1 || !with[0].signalsPresent {
		t.Fatalf("expected one segment with signalsPresent=true, got %+v", with)
	}
	without := parseGuardSegments(json.RawMessage(`[{"text":"a","start":0,"end":1}]`))
	if len(without) != 1 || without[0].signalsPresent {
		t.Fatalf("expected one segment with signalsPresent=false, got %+v", without)
	}
	if got := parseGuardSegments(nil); got != nil {
		t.Errorf("nil segments should parse to nil, got %+v", got)
	}
}
