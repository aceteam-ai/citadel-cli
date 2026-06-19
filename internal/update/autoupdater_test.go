// internal/update/autoupdater_test.go
package update

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChecker is an in-memory ReleaseChecker for tests. No network.
type fakeChecker struct {
	release      *Release
	checkErr     error
	downloadErr  error
	checkCalls   int32
	downloadHits int32
}

func (f *fakeChecker) CheckForUpdate() (*Release, error) {
	atomic.AddInt32(&f.checkCalls, 1)
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	return f.release, nil
}

func (f *fakeChecker) DownloadAndVerify(release *Release, destPath string) error {
	atomic.AddInt32(&f.downloadHits, 1)
	return f.downloadErr
}

func TestParseInterval(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"empty uses default", "", DefaultAutoUpdateInterval, false},
		{"one hour", "1h", time.Hour, false},
		{"thirty minutes", "30m", 30 * time.Minute, false},
		{"invalid", "banana", 0, true},
		{"zero rejected", "0s", 0, true},
		{"negative rejected", "-5m", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInterval(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseInterval(%q) err=%v wantErr=%v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseInterval(%q)=%v want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewAutoUpdaterClampsInterval(t *testing.T) {
	// Below the floor is clamped up.
	u := NewAutoUpdater(AutoUpdaterConfig{Checker: &fakeChecker{}, Interval: time.Second})
	if u.cfg.Interval != MinAutoUpdateInterval {
		t.Errorf("interval below floor not clamped: got %v want %v", u.cfg.Interval, MinAutoUpdateInterval)
	}
	// Zero uses the default.
	u = NewAutoUpdater(AutoUpdaterConfig{Checker: &fakeChecker{}, Interval: 0})
	if u.cfg.Interval != DefaultAutoUpdateInterval {
		t.Errorf("zero interval not defaulted: got %v want %v", u.cfg.Interval, DefaultAutoUpdateInterval)
	}
	// Above the floor is preserved.
	u = NewAutoUpdater(AutoUpdaterConfig{Checker: &fakeChecker{}, Interval: 2 * time.Hour})
	if u.cfg.Interval != 2*time.Hour {
		t.Errorf("valid interval not preserved: got %v", u.cfg.Interval)
	}
}

func TestRunOnce_UpToDate_NoApply(t *testing.T) {
	applied := false
	restarted := false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker: &fakeChecker{release: nil}, // up to date
		Apply:   func(string) error { applied = true; return nil },
		Restart: func() error { restarted = true; return nil },
	})
	if got := u.runOnce(context.Background()); got {
		t.Error("runOnce should not report restart when up to date")
	}
	if applied || restarted {
		t.Errorf("apply/restart must not run when up to date (applied=%v restarted=%v)", applied, restarted)
	}
}

func TestRunOnce_CheckError_NoApply(t *testing.T) {
	applied := false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker: &fakeChecker{checkErr: errors.New("network down")},
		Apply:   func(string) error { applied = true; return nil },
		Restart: func() error { return nil },
	})
	if u.runOnce(context.Background()) {
		t.Error("runOnce should return false on check error")
	}
	if applied {
		t.Error("apply must not run on check error")
	}
}

func TestRunOnce_DownloadError_NoApply(t *testing.T) {
	applied := false
	drained := false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker: &fakeChecker{release: &Release{TagName: "v9.9.9"}, downloadErr: errors.New("checksum mismatch")},
		Apply:   func(string) error { applied = true; return nil },
		Restart: func() error { return nil },
		Drain:   func() { drained = true },
	})
	if u.runOnce(context.Background()) {
		t.Error("runOnce should return false on download error")
	}
	if applied {
		t.Error("apply must not run when download/verify fails")
	}
	if drained {
		t.Error("must not drain before a successful download")
	}
}

func TestRunOnce_HappyPath_DrainsAppliesRestarts(t *testing.T) {
	var order []string
	var mu sync.Mutex
	add := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	drained := false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker:    &fakeChecker{release: &Release{TagName: "v9.9.9"}},
		ActiveJobs: func() int { return 0 }, // idle immediately
		Drain:      func() { drained = true; add("drain") },
		Apply:      func(string) error { add("apply"); return nil },
		Restart:    func() error { add("restart"); return nil },
	})

	if !u.runOnce(context.Background()) {
		t.Fatal("runOnce should report restart on happy path")
	}
	if !drained {
		t.Error("expected drain to be called")
	}
	want := []string{"drain", "apply", "restart"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}
}

func TestRunOnce_DrainsBeforeApply_WaitsForIdle(t *testing.T) {
	// active starts at 1, drops to 0 after Drain is called: verifies the
	// updater waits for in-flight work and only applies once idle.
	var active int32 = 1
	applyCalledWhileBusy := false

	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker:          &fakeChecker{release: &Release{TagName: "v9.9.9"}},
		IdlePollInterval: time.Millisecond,
		IdleTimeout:      2 * time.Second,
		ActiveJobs:       func() int { return int(atomic.LoadInt32(&active)) },
		Drain: func() {
			// Simulate the in-flight job finishing shortly after drain.
			go func() {
				time.Sleep(20 * time.Millisecond)
				atomic.StoreInt32(&active, 0)
			}()
		},
		Apply: func(string) error {
			if atomic.LoadInt32(&active) != 0 {
				applyCalledWhileBusy = true
			}
			return nil
		},
		Restart: func() error { return nil },
	})

	if !u.runOnce(context.Background()) {
		t.Fatal("expected restart on happy path")
	}
	if applyCalledWhileBusy {
		t.Error("apply must not run while jobs are still in flight")
	}
}

func TestRunOnce_IdleTimeout_DefersUpdate(t *testing.T) {
	applied := false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker:          &fakeChecker{release: &Release{TagName: "v9.9.9"}},
		IdlePollInterval: time.Millisecond,
		IdleTimeout:      30 * time.Millisecond,
		ActiveJobs:       func() int { return 1 }, // never idle
		Drain:            func() {},
		Apply:            func(string) error { applied = true; return nil },
		Restart:          func() error { return nil },
	})
	if u.runOnce(context.Background()) {
		t.Error("runOnce should not restart when idle wait times out")
	}
	if applied {
		t.Error("apply must not run if the node never drains")
	}
}

func TestRunOnce_ApplyError_NoRestart(t *testing.T) {
	restarted := false
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker:    &fakeChecker{release: &Release{TagName: "v9.9.9"}},
		ActiveJobs: func() int { return 0 },
		Drain:      func() {},
		Apply:      func(string) error { return errors.New("swap failed") },
		Restart:    func() error { restarted = true; return nil },
	})
	if u.runOnce(context.Background()) {
		t.Error("runOnce should return false when apply fails")
	}
	if restarted {
		t.Error("must not restart when apply fails")
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	u := NewAutoUpdater(AutoUpdaterConfig{
		Checker:  &fakeChecker{release: nil},
		Interval: MinAutoUpdateInterval,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { u.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
