// internal/jobs/meeting_transcribe_rolling.go
//
// Rolling (streaming) transcription driver (issue #5435, epic #5097). The
// shipped notetaker transcribes the recording ONCE at end-of-call. This driver
// adds an additive DURING-call pass: on a fixed cadence it re-transcribes the
// growing recording and emits newly-stabilized transcript segments through a
// callback, so the in-call command monitor (meeting_command.go) can react to
// spoken `/ace` commands live. The end-of-call batch pass remains the source of
// truth for the stored transcript; this is purely additive.
//
// DESIGN — why re-transcribe the whole wav-so-far (v1):
//   - The whisper sidecar transcribes a whole file (no offset/streaming API), and
//     this PR deliberately does not modify that separate Python service. So each
//     pass re-transcribes the recording as it stands. This is correct but its
//     cost grows with call length — late in a long call each pass is slower, so
//     streaming cadence degrades. That is the honest v1 tradeoff; a windowed /
//     offset-based transcription (extract only the trailing unfinished audio) is
//     the named follow-up. The task explicitly endorsed "chunked faster-whisper
//     over the wav-so-far", which this is.
//
// DESIGN — stability margin (why we hold back the tail):
//   - Whole-file re-transcription REVISES the most recent audio: a segment's text
//     and even its start time shift as more context arrives. Emitting the raw
//     tail would surface — and potentially act on — partial or hallucinated
//     text the next pass rewrites. So a segment is only emitted once its end is
//     older than (latest segment end - window). Dedup keys on a coarse
//     start-time bucket so a revised-then-stabilized segment is emitted exactly
//     once.
//
// The driver is pure of whisper/browser specifics: it calls an injected
// transcribeFn returning the CURRENT best full segment list, so it is fully
// unit-testable with a fake (no real whisper in tests).
package jobs

import (
	"fmt"
	"time"
)

// TranscriptSegment is one whisper segment: a [Start,End] time span (seconds
// from the recording start) carrying transcribed text and an optional speaker
// label. Mirrors the sidecar's per-segment JSON shape (see
// transcribe_audio.go's response doc) so a production transcribeFn can decode
// straight into it.
type TranscriptSegment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker,omitempty"`
}

// TranscribeFunc returns the current best full list of segments for the growing
// recording. Implementations re-transcribe the wav-so-far; tests inject a fake
// that returns scripted, growing lists. An error means "this pass failed" — the
// driver logs and continues (a transient whisper hiccup must never crash the
// recording), so a TranscribeFunc should return its own error rather than
// panicking.
type TranscribeFunc func() ([]TranscriptSegment, error)

// SegmentEmitFunc receives each newly-stabilized segment exactly once, in
// increasing start-time order. It runs on the driver's goroutine; keep it fast
// and non-blocking (the production wiring just hands the segment to the
// single-threaded command monitor via a channel).
type SegmentEmitFunc func(TranscriptSegment)

// rollingBucketSeconds is the width of the start-time bucket used to dedup
// segments across re-transcription passes. Whisper start times drift by a
// fraction of a second between passes as context grows; a 1s bucket absorbs that
// jitter without collapsing genuinely distinct adjacent segments (whisper
// segments are multi-second).
const rollingBucketSeconds = 1.0

// RollingTranscriber drives repeated transcription passes and emits stabilized
// segments. It is single-goroutine by construction (Run owns all state), so it
// needs no locking; the caller communicates results out via the emit callback.
type RollingTranscriber struct {
	// Interval is the pass cadence. Must be positive.
	Interval time.Duration
	// Window is the trailing "not yet stable" margin. A segment is withheld
	// until its end is older than (latest end - Window). Must be non-negative.
	Window time.Duration
	// Transcribe is the injected pass function (real whisper or a test fake).
	Transcribe TranscribeFunc
	// Emit receives each stabilized segment once. Required.
	Emit SegmentEmitFunc
	// Log is an optional structured logger for pass errors; nil discards.
	Log func(level, msg string)

	// emittedBuckets records which start-time buckets have already been emitted
	// so a segment revised across passes is surfaced exactly once.
	emittedBuckets map[int64]struct{}
}

// logf logs via the optional Log hook, swallowing output when unset.
func (r *RollingTranscriber) logf(level, format string, args ...any) {
	if r.Log != nil {
		r.Log(level, fmt.Sprintf(format, args...))
	}
}

// Run drives passes on Interval until stop is closed, then performs ONE final
// pass so any segments that stabilized between the last tick and the stop are
// still emitted. It is blocking and meant to run on its own goroutine. Passes
// run SERIALLY (a ticker coalesces if a pass overruns), so two whole-file
// transcriptions never overlap. Run never returns an error: a failed pass is
// logged and skipped, because the recording (the actual deliverable) must
// survive any streaming-layer failure. Note: Run itself does not recover() —
// the production caller wraps the goroutine in a recover (see meeting_join.go)
// so a panic in an injected TranscribeFunc cannot crash the recording.
func (r *RollingTranscriber) Run(stop <-chan struct{}) {
	if r.emittedBuckets == nil {
		r.emittedBuckets = make(map[int64]struct{})
	}
	if r.Interval <= 0 {
		r.logf("warn", "     - rolling transcriber: non-positive interval %v; streaming disabled", r.Interval)
		return
	}
	if r.Transcribe == nil || r.Emit == nil {
		r.logf("warn", "     - rolling transcriber: missing Transcribe or Emit; streaming disabled")
		return
	}

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			// Final pass to catch segments stabilized after the last tick.
			r.runPass()
			return
		case <-ticker.C:
			// PRIORITIZE stop over the ticker: Go's select picks a uniformly
			// random ready case, so once stop is closed a fired ticker could win
			// repeatedly and keep the loop transcribing long after the meeting
			// ended (observed live on node 1084, 2026-07-16: rolling passes still
			// firing 68s after "leaving", starving the end-of-call batch pass).
			// Re-check stop non-blocking so at most the in-flight pass completes,
			// then we do the single final pass and return — never another periodic
			// pass after stop.
			select {
			case <-stop:
				r.runPass()
				return
			default:
			}
			r.runPass()
		}
	}
}

// runPass performs a single transcription pass and emits any segments that have
// newly stabilized (end older than the tail margin, not previously emitted).
func (r *RollingTranscriber) runPass() {
	segments, err := r.Transcribe()
	if err != nil {
		r.logf("warn", "     - rolling transcription pass failed (non-fatal, will retry): %v", err)
		return
	}
	for _, seg := range stabilizedSegments(segments, r.Window, r.emittedBuckets) {
		r.Emit(seg)
	}
}

// stabilizedSegments returns, in start order, the segments that are (a) older
// than the trailing stability margin and (b) not already emitted (tracked by
// coarse start-time bucket in emitted, which this function MUTATES to record the
// newly-emitted buckets). Extracted as a pure function so the emit/dedup/margin
// logic is unit-testable independent of timers and goroutines.
func stabilizedSegments(segments []TranscriptSegment, window time.Duration, emitted map[int64]struct{}) []TranscriptSegment {
	if len(segments) == 0 {
		return nil
	}
	// The tail is the latest segment end across the current pass; everything
	// within `window` of it is still churning and withheld.
	var tail float64
	for _, s := range segments {
		if s.End > tail {
			tail = s.End
		}
	}
	cutoff := tail - window.Seconds()

	var out []TranscriptSegment
	for _, s := range segments {
		// Withhold the churning tail: a segment is stable only once its end is
		// at/under the cutoff. A window of 0 makes every segment stable at once.
		if s.End > cutoff {
			continue
		}
		bucket := startBucket(s.Start)
		if _, seen := emitted[bucket]; seen {
			continue
		}
		emitted[bucket] = struct{}{}
		out = append(out, s)
	}
	return out
}

// startBucket maps a segment start time (seconds) to a coarse integer bucket for
// cross-pass dedup, absorbing the sub-second start-time drift whole-file
// re-transcription introduces.
func startBucket(start float64) int64 {
	return int64(start / rollingBucketSeconds)
}
