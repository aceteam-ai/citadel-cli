package update

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRunOnce_RecordsEveryResult pins that every terminal outcome of a single
// check-download-apply-restart cycle records its bounded result code into
// state.json (citadel-cli#1134). This is the regression guard the issue calls
// for: removing any one record() call from runOnce drops a subtest here.
//
// Each subtest drives runOnce with fakes over a per-test temp state dir and
// asserts the persisted LastResult (and, where relevant, the staged version).
func TestRunOnce_RecordsEveryResult(t *testing.T) {
	release := &Release{TagName: "v9.9.9"}

	t.Run("up_to_date", func(t *testing.T) {
		isolateState(t)
		u := NewAutoUpdater(AutoUpdaterConfig{Checker: &fakeChecker{release: nil}})
		u.runOnce(context.Background())
		assertResult(t, ResultUpToDate)
		if st, _ := LoadState(); st.LastCheckAt.IsZero() {
			t.Error("a completed check must advance LastCheckAt")
		}
	})

	t.Run("check_failed", func(t *testing.T) {
		isolateState(t)
		u := NewAutoUpdater(AutoUpdaterConfig{Checker: &fakeChecker{checkErr: errors.New("offline")}})
		u.runOnce(context.Background())
		assertResult(t, ResultCheckFailed)
	})

	t.Run("manual_update_required (homebrew)", func(t *testing.T) {
		isolateState(t)
		u := NewAutoUpdater(AutoUpdaterConfig{
			Checker:         &fakeChecker{release: release},
			HomebrewManaged: func() bool { return true },
		})
		u.runOnce(context.Background())
		assertResult(t, ResultManualUpdateRequired)
		if st, _ := LoadState(); st.LatestVersion != "v9.9.9" {
			t.Errorf("latest observed release = %q, want v9.9.9", st.LatestVersion)
		}
	})

	t.Run("download_failed", func(t *testing.T) {
		isolateState(t)
		u := NewAutoUpdater(AutoUpdaterConfig{
			Checker:    &fakeChecker{release: release, downloadErr: errors.New("checksum mismatch")},
			BeginDrain: func() func() { return func() {} },
		})
		u.runOnce(context.Background())
		assertResult(t, ResultDownloadFailed)
	})

	t.Run("drain_deferred", func(t *testing.T) {
		isolateState(t)
		u := NewAutoUpdater(AutoUpdaterConfig{
			Checker:          &fakeChecker{release: release},
			IdlePollInterval: time.Millisecond,
			IdleTimeout:      30 * time.Millisecond,
			ActiveJobs:       func() int { return 1 }, // never idle
			BeginDrain:       func() func() { return func() {} },
			Apply:            func(string) error { return nil },
		})
		u.runOnce(context.Background())
		assertResult(t, ResultDrainDeferred)
	})

	t.Run("apply_failed", func(t *testing.T) {
		isolateState(t)
		u := NewAutoUpdater(AutoUpdaterConfig{
			Checker:    &fakeChecker{release: release},
			ActiveJobs: func() int { return 0 },
			BeginDrain: func() func() { return func() {} },
			Apply:      func(string) error { return errors.New("swap failed") },
		})
		u.runOnce(context.Background())
		assertResult(t, ResultApplyFailed)
	})

	t.Run("restart_pending on applied+restarted", func(t *testing.T) {
		isolateState(t)
		u := NewAutoUpdater(AutoUpdaterConfig{
			Checker:    &fakeChecker{release: release},
			ActiveJobs: func() int { return 0 },
			BeginDrain: func() func() { return func() {} },
			Apply:      func(string) error { return nil },
			Restart:    func() error { return nil },
		})
		if !u.runOnce(context.Background()) {
			t.Error("runOnce should report restart requested on the happy path")
		}
		// A successful apply records STAGING (restart_pending), not proof the new
		// process is running.
		assertResult(t, ResultRestartPending)
		if st, _ := LoadState(); st.CurrentVersion != "v9.9.9" {
			t.Errorf("staged version = %q, want v9.9.9", st.CurrentVersion)
		}
	})

	t.Run("restart_failed retains old running + pending new", func(t *testing.T) {
		isolateState(t)
		if err := SaveState(&State{CurrentVersion: "v1.0.0", AutoUpdate: true, Channel: "stable"}); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		u := NewAutoUpdater(AutoUpdaterConfig{
			Checker:    &fakeChecker{release: release},
			ActiveJobs: func() int { return 0 },
			BeginDrain: func() func() { return func() {} },
			Apply:      func(string) error { return nil },
			Restart:    func() error { return errors.New("exec failed") },
		})
		if u.runOnce(context.Background()) {
			t.Error("runOnce must return false when restart fails")
		}
		assertResult(t, ResultRestartFailed)
		st, _ := LoadState()
		if st.CurrentVersion != "v9.9.9" {
			t.Errorf("staged (pending) version = %q, want v9.9.9", st.CurrentVersion)
		}
		if st.PreviousVersion != "v1.0.0" {
			t.Errorf("previous (still-running) version = %q, want v1.0.0", st.PreviousVersion)
		}
		if !st.AutoUpdate {
			t.Error("preference clobbered by result writes")
		}
	})
}

func assertResult(t *testing.T, want string) {
	t.Helper()
	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.LastResult != want {
		t.Fatalf("LastResult = %q, want %q", st.LastResult, want)
	}
	if st.LastResultAt.IsZero() {
		t.Error("LastResultAt not set alongside LastResult")
	}
}
