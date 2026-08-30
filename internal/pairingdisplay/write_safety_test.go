package pairingdisplay

import (
	"errors"
	"strings"
	"testing"
)

// recordingFailWriter is an io.Writer that fails the Nth call (1-indexed;
// 0 means never fail) and records every call's bytes, for testing
// writeFrameOrClear without a real console.
type recordingFailWriter struct {
	failOnCall int
	calls      []string
}

func (w *recordingFailWriter) Write(p []byte) (int, error) {
	w.calls = append(w.calls, string(p))
	if w.failOnCall != 0 && len(w.calls) == w.failOnCall {
		return 0, errors.New("boom")
	}
	return len(p), nil
}

func TestWriteFrameOrClear_SuccessMakesNoClearAttempt(t *testing.T) {
	w := &recordingFailWriter{}
	if err := writeFrameOrClear(w, "the frame"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(w.calls) != 1 {
		t.Fatalf("expected exactly 1 write on success, got %d: %v", len(w.calls), w.calls)
	}
	if w.calls[0] != "the frame" {
		t.Fatalf("unexpected write content: %q", w.calls[0])
	}
}

func TestWriteFrameOrClear_FailureAttemptsClearAndReturnsOriginalError(t *testing.T) {
	// Pins the fix for a review-caught gap: Manager arms no TTL/crash-marker
	// cleanup for a non-delivered Show outcome, so a partial/failed write
	// must reclaim the console itself right here or a code fragment could
	// be left on the physical screen indefinitely.
	w := &recordingFailWriter{failOnCall: 1}
	err := writeFrameOrClear(w, "the frame")
	if err == nil {
		t.Fatalf("expected the original write error to be returned")
	}
	if len(w.calls) != 2 {
		t.Fatalf("expected a clear-frame retry after the failed write, got %d calls: %v", len(w.calls), w.calls)
	}
	if !strings.Contains(w.calls[1], ansiClearHome) {
		t.Fatalf("expected the second write to be a clear frame (clear+home), got %q", w.calls[1])
	}
}

func TestWriteFrameOrClear_BothWritesFailStillReturnsOriginalError(t *testing.T) {
	// The clear attempt is best-effort: if it ALSO fails, there is nothing
	// further to try locally, but the caller must still see a non-nil error
	// so it fails closed (delivered:false) rather than silently succeeding.
	w := &alwaysFailWriter{}
	err := writeFrameOrClear(w, "the frame")
	if err == nil {
		t.Fatalf("expected an error even when both the render and the clear attempt fail")
	}
	if w.calls != 2 {
		t.Fatalf("expected both the original write and the clear-attempt retry, got %d calls", w.calls)
	}
}

// alwaysFailWriter fails every write, unconditionally, so both the original
// write and the best-effort clear attempt in writeFrameOrClear fail.
type alwaysFailWriter struct{ calls int }

func (a *alwaysFailWriter) Write(p []byte) (int, error) {
	a.calls++
	return 0, errors.New("boom")
}
