package heartbeat

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMarkerMissingFileReturnsZeroValue(t *testing.T) {
	dir := t.TempDir()

	m := LoadMarker(dir)
	if !m.LastSuccessAt.IsZero() {
		t.Fatalf("expected zero LastSuccessAt for missing marker, got %v", m.LastSuccessAt)
	}
	if m.ConsecutiveFailures != 0 {
		t.Fatalf("expected 0 ConsecutiveFailures, got %d", m.ConsecutiveFailures)
	}

	state, age := Freshness(m, time.Now(), DefaultStaleAfter)
	if state != FreshnessUnknown {
		t.Fatalf("expected FreshnessUnknown for a never-written marker, got %v", state)
	}
	if age != 0 {
		t.Fatalf("expected zero age for FreshnessUnknown, got %v", age)
	}
}

func TestRecordSuccessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	if err := RecordSuccess(dir, now); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}

	m := LoadMarker(dir)
	if !m.LastSuccessAt.Equal(now) {
		t.Fatalf("LastSuccessAt = %v, want %v", m.LastSuccessAt, now)
	}
	if !m.LastAttemptAt.Equal(now) {
		t.Fatalf("LastAttemptAt = %v, want %v", m.LastAttemptAt, now)
	}
	if m.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures reset to 0, got %d", m.ConsecutiveFailures)
	}
	if m.LastError != "" {
		t.Fatalf("expected LastError cleared, got %q", m.LastError)
	}

	// File is named heartbeat_marker.yaml, mirroring energy.yaml/telemetry.yaml.
	if _, err := filepath.Glob(filepath.Join(dir, markerFile)); err != nil {
		t.Fatalf("glob: %v", err)
	}

	state, age := Freshness(m, now, DefaultStaleAfter)
	if state != FreshnessOK {
		t.Fatalf("expected FreshnessOK immediately after success, got %v", state)
	}
	if age != 0 {
		t.Fatalf("expected zero age at the moment of success, got %v", age)
	}
}

func TestRecordFailureIncrementsConsecutiveCountAndPreservesLastSuccess(t *testing.T) {
	dir := t.TempDir()
	success := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)

	if err := RecordSuccess(dir, success); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}

	fail1 := success.Add(30 * time.Second)
	if err := RecordFailure(dir, fail1, errors.New("connection refused")); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	fail2 := fail1.Add(30 * time.Second)
	if err := RecordFailure(dir, fail2, errors.New("connection refused")); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	m := LoadMarker(dir)
	// LastSuccessAt must survive subsequent failures -- it is the durable
	// "last known good" signal, not overwritten by attempts that did not land.
	if !m.LastSuccessAt.Equal(success) {
		t.Fatalf("LastSuccessAt = %v, want %v (must not move on failure)", m.LastSuccessAt, success)
	}
	if !m.LastAttemptAt.Equal(fail2) {
		t.Fatalf("LastAttemptAt = %v, want %v", m.LastAttemptAt, fail2)
	}
	if m.ConsecutiveFailures != 2 {
		t.Fatalf("ConsecutiveFailures = %d, want 2", m.ConsecutiveFailures)
	}
	if m.LastError != "connection refused" {
		t.Fatalf("LastError = %q, want %q", m.LastError, "connection refused")
	}

	// Evaluated as of real "now" (~10 minutes after success, past
	// DefaultStaleAfter), the marker reads stale even though attempts are
	// still happening -- that gap (attempts continuing, none landing) is
	// exactly the signal #726 asks for.
	now := time.Now()
	state, age := Freshness(m, now, DefaultStaleAfter)
	if state != FreshnessStale {
		t.Fatalf("expected FreshnessStale, got %v", state)
	}
	if age != now.Sub(success) {
		t.Fatalf("age = %v, want %v", age, now.Sub(success))
	}
}

func TestRecordSuccessAfterFailuresResetsCounters(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().UTC().Truncate(time.Second)

	if err := RecordFailure(dir, base, errors.New("timeout")); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := RecordFailure(dir, base.Add(time.Second), errors.New("timeout")); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	recovered := base.Add(2 * time.Second)
	if err := RecordSuccess(dir, recovered); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}

	m := LoadMarker(dir)
	if m.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures reset to 0 after recovery, got %d", m.ConsecutiveFailures)
	}
	if m.LastError != "" {
		t.Fatalf("expected LastError cleared after recovery, got %q", m.LastError)
	}
	if !m.LastSuccessAt.Equal(recovered) {
		t.Fatalf("LastSuccessAt = %v, want %v", m.LastSuccessAt, recovered)
	}
}

func TestFreshnessBoundaryIsExclusive(t *testing.T) {
	dir := t.TempDir()
	success := time.Now().UTC().Truncate(time.Second)
	if err := RecordSuccess(dir, success); err != nil {
		t.Fatalf("RecordSuccess: %v", err)
	}
	m := LoadMarker(dir)

	// Exactly at the threshold: not yet stale ("> staleAfter", not ">=").
	atThreshold := success.Add(DefaultStaleAfter)
	if state, _ := Freshness(m, atThreshold, DefaultStaleAfter); state != FreshnessOK {
		t.Fatalf("expected FreshnessOK exactly at the threshold, got %v", state)
	}

	// One nanosecond past: stale.
	pastThreshold := atThreshold.Add(time.Nanosecond)
	if state, _ := Freshness(m, pastThreshold, DefaultStaleAfter); state != FreshnessStale {
		t.Fatalf("expected FreshnessStale just past the threshold, got %v", state)
	}
}
