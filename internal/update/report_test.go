package update

import (
	"os"
	"sync"
	"testing"
	"time"
)

// isolateState points the state file at a per-test temp dir. TestMain already
// sandboxes HOME for the whole package; this gives each test its own clean file
// so writes don't leak between tests.
func isolateState(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestReadStateForReport(t *testing.T) {
	t.Run("missing is default, not corrupt", func(t *testing.T) {
		isolateState(t)
		st, corrupt := ReadStateForReport()
		if corrupt {
			t.Fatal("absent state must not read as corrupt")
		}
		if st == nil || st.AutoUpdate {
			t.Fatalf("absent state must be default-off, got %+v", st)
		}
	})

	t.Run("valid reads cleanly", func(t *testing.T) {
		isolateState(t)
		if err := SaveState(&State{AutoUpdate: true, Channel: "stable", CurrentVersion: "v2.0.0"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		st, corrupt := ReadStateForReport()
		if corrupt || st == nil || !st.AutoUpdate || st.CurrentVersion != "v2.0.0" {
			t.Fatalf("valid state misread: corrupt=%v st=%+v", corrupt, st)
		}
	})

	t.Run("corrupt is unknown, never a confident default", func(t *testing.T) {
		isolateState(t)
		if err := EnsureUpdateDir(); err != nil {
			t.Fatalf("EnsureUpdateDir: %v", err)
		}
		if err := os.WriteFile(GetStateFilePath(), []byte("{not valid json"), 0644); err != nil {
			t.Fatalf("write corrupt: %v", err)
		}
		st, corrupt := ReadStateForReport()
		if !corrupt || st != nil {
			t.Fatalf("corrupt state must read as (nil, true), got st=%+v corrupt=%v", st, corrupt)
		}
	})
}

// TestRecordCheckResultPreservesPreference pins that a telemetry result write
// never clobbers the persisted preference (citadel-cli#1134): an opted-out node
// stays opted out across an interleaved result write.
func TestRecordCheckResultPreservesPreference(t *testing.T) {
	isolateState(t)
	if err := SaveState(&State{AutoUpdate: false, Channel: "stable", CurrentVersion: "v2.0.0"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	now := time.Unix(1000, 0)
	if err := RecordCheckResult(now, ResultUpToDate, true, "v2.0.0"); err != nil {
		t.Fatalf("RecordCheckResult: %v", err)
	}
	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.AutoUpdate {
		t.Error("persisted opt-out was clobbered by a result write")
	}
	if st.Channel != "stable" || st.CurrentVersion != "v2.0.0" {
		t.Errorf("non-telemetry fields clobbered: %+v", st)
	}
	if st.LastResult != ResultUpToDate {
		t.Errorf("LastResult = %q, want %q", st.LastResult, ResultUpToDate)
	}
	if st.LastCheckAt.IsZero() {
		t.Error("checked=true must advance LastCheckAt")
	}
	if st.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want v2.0.0", st.LatestVersion)
	}
}

// TestMutateStateConcurrentBoundary exercises the tested concurrency boundary
// (citadel-cli#1134): a single preference write (AutoUpdate=true) racing many
// result writes must never be lost. The stateMu load-modify-save guarantees
// that once the preference lands, every later result write preserves it; drop
// the mutex and a concurrent save would clobber it (caught under -race / the
// 1000-iteration loop). Afterward both a preference and a result must be set.
func TestMutateStateConcurrentBoundary(t *testing.T) {
	isolateState(t)
	if err := SaveState(&State{Channel: "stable"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var wg sync.WaitGroup
	// Many result writers.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = RecordCheckResult(time.Unix(1, 0), ResultCheckFailed, true, "")
		}()
	}
	// A preference writer (the `citadel update enable` analogue), run through the
	// same atomic path a few times so it interleaves with the result writers.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mutateState(func(s *State) { s.AutoUpdate = true })
		}()
	}
	wg.Wait()

	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.AutoUpdate {
		t.Error("preference write was lost under concurrent result writes (lost update)")
	}
	if st.LastResult == "" {
		t.Error("no result recorded")
	}
}

func TestRecordStagedUpdate(t *testing.T) {
	isolateState(t)
	if err := SaveState(&State{CurrentVersion: "v1.0.0", AutoUpdate: true, Channel: "stable"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := RecordStagedUpdate(time.Unix(2000, 0), "v2.0.0"); err != nil {
		t.Fatalf("RecordStagedUpdate: %v", err)
	}
	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.CurrentVersion != "v2.0.0" || st.PreviousVersion != "v1.0.0" {
		t.Errorf("version staging wrong: cur=%q prev=%q", st.CurrentVersion, st.PreviousVersion)
	}
	if st.LastResult != ResultRestartPending {
		t.Errorf("LastResult = %q, want %q", st.LastResult, ResultRestartPending)
	}
	if st.LatestVersion != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want v2.0.0", st.LatestVersion)
	}
	if !st.AutoUpdate || st.Channel != "stable" {
		t.Errorf("preference fields not preserved: %+v", st)
	}
}

// TestReconcilePendingOnStartup pins the next-process reconcile: pending clears
// to up_to_date ONLY when the staged version now matches the running binary;
// a still-mismatched version (restart failed) retains its result.
func TestReconcilePendingOnStartup(t *testing.T) {
	t.Run("match clears restart_pending", func(t *testing.T) {
		isolateState(t)
		if err := SaveState(&State{CurrentVersion: "v2.0.0", LastResult: ResultRestartPending, AutoUpdate: true, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		if err := ReconcilePendingOnStartup(time.Unix(3000, 0), "v2.0.0"); err != nil {
			t.Fatalf("ReconcilePendingOnStartup: %v", err)
		}
		st, _ := LoadState()
		if st.LastResult != ResultUpToDate {
			t.Errorf("matched restart should clear to up_to_date, got %q", st.LastResult)
		}
		if !st.AutoUpdate {
			t.Error("preference clobbered during reconcile")
		}
	})

	t.Run("mismatch retains restart_failed", func(t *testing.T) {
		isolateState(t)
		if err := SaveState(&State{CurrentVersion: "v9.9.9", LastResult: ResultRestartFailed}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		if err := ReconcilePendingOnStartup(time.Unix(3000, 0), "v2.0.0"); err != nil {
			t.Fatalf("ReconcilePendingOnStartup: %v", err)
		}
		st, _ := LoadState()
		if st.LastResult != ResultRestartFailed {
			t.Errorf("mismatched restart must retain its result, got %q", st.LastResult)
		}
	})

	t.Run("non-pending result untouched even on match", func(t *testing.T) {
		isolateState(t)
		if err := SaveState(&State{CurrentVersion: "v2.0.0", LastResult: ResultCheckFailed}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		if err := ReconcilePendingOnStartup(time.Unix(3000, 0), "v2.0.0"); err != nil {
			t.Fatalf("ReconcilePendingOnStartup: %v", err)
		}
		st, _ := LoadState()
		if st.LastResult != ResultCheckFailed {
			t.Errorf("non-pending result must be left alone, got %q", st.LastResult)
		}
	})
}
