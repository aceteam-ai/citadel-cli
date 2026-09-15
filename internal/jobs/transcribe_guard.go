// internal/jobs/transcribe_guard.go
package jobs

import (
	"encoding/json"
	"strings"
)

// The node-side "no intelligible speech / low confidence" guard (citadel#1045).
//
// The whisper sidecar is the SENSOR: it produces per-segment probabilities
// (no_speech_prob, avg_logprob, compression_ratio) that faster-whisper already
// computes internally. This file is the POLICY: a pure, deterministic verdict
// over those signals plus the transcript text, attached ADDITIVELY to the
// relayed result so a caller can tell a real "Thank you very much." from a
// hallucinated one. Keeping the policy here (Go), not in the Python sidecar,
// makes it unit-testable in CI without a running whisper container and gives
// the fleet a single source of truth for the verdict.
//
// The verdict is always computed (it is the whole point of the fix), but it is
// purely ADVISORY and ADDITIVE: it never alters the transcript text/segments,
// and the transcription itself is byte-identical to before when the caller
// passes none of the new tuning params.
//
// Thresholds mirror faster-whisper 1.0.3's own decoding defaults so the node's
// advisory verdict lines up with the engine's internal skip rules. They are
// package consts (not payload-driven) so the verdict is stable across callers;
// the payload's own no_speech_threshold / logprob_threshold /
// compression_ratio_threshold tune the ENGINE's decoding, not this guard.
const (
	// guardNoSpeechProbThreshold: mean no_speech_prob at/above this reads as
	// "the model itself thinks there is no speech here" (faster-whisper
	// no_speech_threshold default).
	guardNoSpeechProbThreshold = 0.6
	// guardLogprobThreshold: mean avg_logprob at/below this reads as
	// "low-confidence tokens" (faster-whisper log_prob_threshold default).
	guardLogprobThreshold = -1.0
	// guardCompressionRatioThreshold: a segment's gzip compression ratio
	// at/above this is the classic degenerate-repetition tell (faster-whisper
	// compression_ratio_threshold default).
	guardCompressionRatioThreshold = 2.4
	// guardLanguageProbabilityFloor: an auto-detected language below this
	// confidence (with no caller language hint) is the "misdetected as Welsh"
	// case. Works even against an old sidecar image, since language_probability
	// already exists in the response.
	guardLanguageProbabilityFloor = 0.5

	// Repetition detection is signal-INDEPENDENT (pure text), so it also
	// catches the looped-filler hallucination against an old sidecar image that
	// does not yet emit per-segment signals.
	guardMinSegmentsForRepetition = 4
	guardRepetitionDistinctRatio  = 0.5
	guardDominantPhraseFraction   = 0.6
	// guardMinRepeatedPhraseWords: the repeated phrase must be at least this
	// many words for a loop to register, so a backchannel-heavy transcript
	// ("Yeah." / "Okay." / "Mm-hmm.") is never mislabeled a hallucination loop.
	// "Thank you very much." / "It's a pleasure to meet you." / "Diolch yn fawr"
	// all clear it.
	guardMinRepeatedPhraseWords = 3

	// guardMinSpeechCoverage: the fraction of the recording's DURATION covered
	// by transcribed segments below which the transcript is treated as
	// no-speech. This catches the incident's own signature — one short segment
	// ("Thank you very much.") standing in for a 38-minute recording — even when
	// that lone segment's probabilities happen to sit inside faster-whisper's
	// own thresholds. Deliberately very conservative (2%): only a recording that
	// produced almost NO transcribed speech trips it. Fail-open when duration is
	// unknown (0).
	guardMinSpeechCoverage = 0.02
)

// guardSegment is the per-segment signal subset the guard reasons over.
// signalsPresent records whether the sidecar actually emitted the probability
// fields for this segment (an old image does not), so an absent signal is never
// mistaken for a confident zero.
type guardSegment struct {
	Text             string
	Start            float64
	End              float64
	NoSpeechProb     float64
	AvgLogprob       float64
	CompressionRatio float64
	signalsPresent   bool
}

// TranscriptionGuard is the advisory verdict attached to a transcribe result as
// the additive `transcription_guard` object. All fields are additive; a
// consumer that does not know about it simply ignores it.
type TranscriptionGuard struct {
	// NoSpeech: the node found no intelligible speech (empty transcript, or the
	// model's own no_speech probability is high). A caller should treat the
	// transcript as unreliable rather than as content.
	NoSpeech bool `json:"no_speech"`
	// LowConfidence: the transcript may be unreliable (low token confidence,
	// a repetition loop, an uncertain detected language, or NoSpeech).
	LowConfidence bool `json:"low_confidence"`
	// Repetition: a degenerate repeated-phrase loop was detected.
	Repetition bool `json:"repetition"`
	// Reasons: machine-readable reason codes for the flags above.
	Reasons []string `json:"reasons"`
	// SignalsAvailable: whether the sidecar emitted per-segment probabilities.
	// When false, NoSpeech/LowConfidence rest ONLY on the signal-independent
	// checks (empty transcript, repetition, uncertain language) — the guard
	// never asserts a confident NoSpeech from absent signals.
	SignalsAvailable bool `json:"signals_available"`
	// Aggregate signal values (means / max), for observability. Zero when no
	// signals were available.
	AvgLogprob          float64 `json:"avg_logprob"`
	NoSpeechProb        float64 `json:"no_speech_prob"`
	CompressionRatio    float64 `json:"compression_ratio"`
	LanguageProbability float64 `json:"language_probability"`
	// SpeechCoverage is transcribed-seconds / recording-duration, for
	// observability. -1 when duration is unknown (never computed).
	SpeechCoverage float64 `json:"speech_coverage"`
}

// reason codes.
const (
	guardReasonEmptyTranscript = "empty_transcript"
	guardReasonHighNoSpeech    = "high_no_speech_probability"
	guardReasonLowLogprob      = "low_avg_logprob"
	guardReasonHighCompression = "high_compression_ratio"
	guardReasonRepetitionLoop  = "repetition_loop"
	guardReasonUncertainLang   = "uncertain_language"
	guardReasonLowCoverage     = "low_speech_coverage"
)

// evaluateTranscriptionGuard is the pure verdict function. It takes the parsed
// per-segment signals, the full transcript text, the detected-language
// probability, whether the caller supplied a language hint, and the recording
// duration in seconds (0 = unknown). It never touches the network or the
// filesystem.
func evaluateTranscriptionGuard(segs []guardSegment, fullText string, languageProbability float64, languageHinted bool, durationSeconds float64) TranscriptionGuard {
	g := TranscriptionGuard{
		Reasons:             []string{},
		LanguageProbability: languageProbability,
		SpeechCoverage:      -1,
	}

	text := strings.TrimSpace(fullText)
	if text == "" {
		text = strings.TrimSpace(joinSegmentText(segs))
	}

	// Empty transcript is the unambiguous no-speech case; it needs no signals.
	if text == "" {
		g.NoSpeech = true
		g.LowConfidence = true
		g.Reasons = append(g.Reasons, guardReasonEmptyTranscript)
		return g
	}

	// Aggregate the per-segment signals over the segments that actually carry
	// them. Mean for probabilities/logprob; max for compression ratio (a single
	// looped segment is enough to condemn the pass).
	var sumNoSpeech, sumLogprob float64
	var maxCompression float64
	var n int
	for _, s := range segs {
		if !s.signalsPresent {
			continue
		}
		n++
		sumNoSpeech += s.NoSpeechProb
		sumLogprob += s.AvgLogprob
		if s.CompressionRatio > maxCompression {
			maxCompression = s.CompressionRatio
		}
	}
	g.SignalsAvailable = n > 0
	if g.SignalsAvailable {
		g.NoSpeechProb = sumNoSpeech / float64(n)
		g.AvgLogprob = sumLogprob / float64(n)
		g.CompressionRatio = maxCompression
	}

	// Signal-based flags (only when the sidecar emitted signals).
	if g.SignalsAvailable {
		if g.NoSpeechProb >= guardNoSpeechProbThreshold {
			g.NoSpeech = true
			g.Reasons = append(g.Reasons, guardReasonHighNoSpeech)
		}
		if g.AvgLogprob <= guardLogprobThreshold {
			g.LowConfidence = true
			g.Reasons = append(g.Reasons, guardReasonLowLogprob)
		}
		if g.CompressionRatio >= guardCompressionRatioThreshold {
			g.Repetition = true
			g.LowConfidence = true
			g.Reasons = append(g.Reasons, guardReasonHighCompression)
		}
	}

	// Signal-independent repetition detection (works pre-rebuild).
	if detectRepetitionLoop(segs) {
		g.Repetition = true
		g.LowConfidence = true
		g.Reasons = append(g.Reasons, guardReasonRepetitionLoop)
	}

	// Signal-independent low-speech-coverage detection (the incident's own
	// signature: one short segment standing in for a long recording). Only
	// evaluated when the sidecar reported a positive duration — fail-open when
	// it is unknown. This uses only segment timings + duration, so it works
	// against an old sidecar image too.
	if durationSeconds > 0 {
		var speech float64
		for _, s := range segs {
			if d := s.End - s.Start; d > 0 {
				speech += d
			}
		}
		g.SpeechCoverage = speech / durationSeconds
		if g.SpeechCoverage < guardMinSpeechCoverage {
			g.NoSpeech = true
			g.LowConfidence = true
			g.Reasons = append(g.Reasons, guardReasonLowCoverage)
		}
	}

	// Uncertain detected language with no caller hint (the "misdetected as
	// Welsh" case). Only meaningful when a probability was actually reported
	// (>0); a missing field (0) is not treated as low confidence.
	if !languageHinted && languageProbability > 0 && languageProbability < guardLanguageProbabilityFloor {
		g.LowConfidence = true
		g.Reasons = append(g.Reasons, guardReasonUncertainLang)
	}

	// A no-speech verdict inherently means the transcript is not trustworthy
	// content.
	if g.NoSpeech {
		g.LowConfidence = true
	}

	return g
}

// joinSegmentText reconstructs the full transcript from segment texts when the
// response carries no top-level `text` field.
func joinSegmentText(segs []guardSegment) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		if t := strings.TrimSpace(s.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// normalizePhrase lowercases and collapses whitespace so trivially-different
// renderings of the same looped phrase compare equal.
func normalizePhrase(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// detectRepetitionLoop reports a degenerate repeated-phrase loop across
// segments using only the text (no probability signals). It fires when either
// a single phrase dominates the transcript or the distinct-phrase ratio is low,
// both gated on (a) at least guardMinSegmentsForRepetition segments and (b) the
// repeated content being a real phrase (>= guardMinRepeatedPhraseWords words) —
// so a short or backchannel-heavy transcript ("Yeah." / "Okay." / "Mm-hmm.") is
// never mislabeled a hallucination loop. It deliberately does NOT try to catch a
// single segment whose OWN text repeats internally — that is what the
// compression-ratio signal covers when signals are available, and a text-only
// heuristic there risks false positives on legitimately repetitive speech.
func detectRepetitionLoop(segs []guardSegment) bool {
	phrases := make([]string, 0, len(segs))
	for _, s := range segs {
		if p := normalizePhrase(s.Text); p != "" {
			phrases = append(phrases, p)
		}
	}
	total := len(phrases)
	if total < guardMinSegmentsForRepetition {
		return false
	}

	counts := make(map[string]int, total)
	var maxCount int
	var maxPhrase string
	for _, p := range phrases {
		counts[p]++
		if counts[p] > maxCount {
			maxCount = counts[p]
			maxPhrase = p
		}
	}

	// The repeated content must be substantial, or a run of short backchannels
	// would register as a loop.
	if len(strings.Fields(maxPhrase)) < guardMinRepeatedPhraseWords {
		return false
	}

	// Dominant single phrase (e.g. "It's a pleasure to meet you." looped).
	if float64(maxCount)/float64(total) >= guardDominantPhraseFraction {
		return true
	}

	// Low distinct-phrase ratio (e.g. a Welsh gibberish loop cycling a handful
	// of phrases).
	if float64(len(counts))/float64(total) <= guardRepetitionDistinctRatio {
		return true
	}
	return false
}

// attachTranscriptionGuard parses a sidecar transcribe response, computes the
// advisory guard, and re-emits the JSON with an additive `transcription_guard`
// object. Every OTHER field is VALUE-preserved: the top level is decoded as raw
// messages, so no field's VALUE changes and the transcript's own floats are
// never re-serialized (json.Marshal does compact the raw messages — stripping
// insignificant whitespace and HTML-escaping strings — but values are intact).
// On any parse/marshal failure it returns the original bytes unchanged —
// relaying verbatim, exactly as the handler did before this fix.
func attachTranscriptionGuard(raw []byte, languageHinted bool) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return raw
	}
	// A well-formed response is a JSON object; anything else (array, scalar)
	// unmarshals to a nil map — leave it untouched.
	if top == nil {
		return raw
	}
	// Never clobber a field the sidecar already named this.
	if _, exists := top["transcription_guard"]; exists {
		return raw
	}

	var fullText string
	if rm, ok := top["text"]; ok {
		_ = json.Unmarshal(rm, &fullText)
	}
	var languageProbability float64
	if rm, ok := top["language_probability"]; ok {
		_ = json.Unmarshal(rm, &languageProbability)
	}
	var durationSeconds float64
	if rm, ok := top["duration"]; ok {
		_ = json.Unmarshal(rm, &durationSeconds)
	}

	segs := parseGuardSegments(top["segments"])

	guard := evaluateTranscriptionGuard(segs, fullText, languageProbability, languageHinted, durationSeconds)
	guardJSON, err := json.Marshal(guard)
	if err != nil {
		return raw
	}
	top["transcription_guard"] = guardJSON

	out, err := json.Marshal(top)
	if err != nil {
		return raw
	}
	return out
}

// parseGuardSegments extracts per-segment signals from the raw `segments` array,
// tracking (per segment) whether the probability fields were actually present.
func parseGuardSegments(rawSegments json.RawMessage) []guardSegment {
	if len(rawSegments) == 0 {
		return nil
	}
	var rawSegs []map[string]json.RawMessage
	if err := json.Unmarshal(rawSegments, &rawSegs); err != nil {
		return nil
	}
	segs := make([]guardSegment, 0, len(rawSegs))
	for _, rs := range rawSegs {
		var gs guardSegment
		if rm, ok := rs["text"]; ok {
			_ = json.Unmarshal(rm, &gs.Text)
		}
		if rm, ok := rs["start"]; ok {
			_ = json.Unmarshal(rm, &gs.Start)
		}
		if rm, ok := rs["end"]; ok {
			_ = json.Unmarshal(rm, &gs.End)
		}
		present := false
		if rm, ok := rs["no_speech_prob"]; ok {
			if json.Unmarshal(rm, &gs.NoSpeechProb) == nil {
				present = true
			}
		}
		if rm, ok := rs["avg_logprob"]; ok {
			if json.Unmarshal(rm, &gs.AvgLogprob) == nil {
				present = true
			}
		}
		if rm, ok := rs["compression_ratio"]; ok {
			if json.Unmarshal(rm, &gs.CompressionRatio) == nil {
				present = true
			}
		}
		gs.signalsPresent = present
		segs = append(segs, gs)
	}
	return segs
}
