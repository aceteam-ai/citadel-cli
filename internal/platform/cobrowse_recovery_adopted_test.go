package platform

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Every PID is synthetic and every signal is intercepted. Manual observations
// drive the production adapter; no browser, process, scope or CDP is started.
type adoptedRecoveryFixture struct {
	mu        sync.Mutex
	alive     map[int]bool
	starts    map[int]uint64
	reads     map[int]int
	faultPID  int
	fault     error
	faultRead int
	kills     []int
	killed    chan int
	ticks     chan time.Time
	done      chan struct{}
	proc      *cobrowseProc
	manager   *CobrowseSessionManager
	session   *cobrowseSession
}

const recoveryFakeBrowser = 987654321
const recoveryFakeXvfb = 987654322

func newAdoptedRecoveryFixture(t *testing.T, companion bool) *adoptedRecoveryFixture {
	t.Helper()
	f := &adoptedRecoveryFixture{
		alive:  map[int]bool{recoveryFakeBrowser: true},
		starts: map[int]uint64{recoveryFakeBrowser: 111},
		reads:  map[int]int{}, killed: make(chan int, 4),
		ticks: make(chan time.Time), done: make(chan struct{}),
	}
	rec := sessionRecord{ID: "cb-adopted-recovery", BrowserPID: recoveryFakeBrowser, BrowserStart: 111}
	if companion {
		f.alive[recoveryFakeXvfb], f.starts[recoveryFakeXvfb] = true, 222
		rec.XvfbPID, rec.XvfbStart = recoveryFakeXvfb, 222
	}
	oldIdentity, oldKill := procIdentity, pidKiller
	procIdentity = func(pid int) (uint64, bool, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reads[pid]++
		if pid == f.faultPID && (f.faultRead == 0 || f.reads[pid] == f.faultRead) {
			return 0, false, f.fault
		}
		return f.starts[pid], f.alive[pid], nil
	}
	pidKiller = func(pid int) error {
		f.mu.Lock()
		f.kills = append(f.kills, pid)
		f.alive[pid] = false
		f.mu.Unlock()
		f.killed <- pid
		return nil
	}
	// Restore globals only after the observer and every stop call are done.
	t.Cleanup(func() { procIdentity, pidKiller = oldIdentity, oldKill })
	f.proc = adoptedProcWithObservations(rec, f.ticks, func() { close(f.done) })
	t.Cleanup(func() {
		close(f.ticks)
		select {
		case <-f.done:
		case <-time.After(time.Second):
			t.Error("adopted observer leaked")
		}
	})
	f.manager = newCobrowseSessionManager(trustedBaseDir(t), 2)
	f.manager.Recover()
	profile := filepath.Join(f.manager.baseDir, rec.ID)
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	f.session = &cobrowseSession{id: rec.ID, profile: profile, state: SessionRunning, proc: f.proc}
	f.manager.sessions[rec.ID] = f.session
	return f
}

func (f *adoptedRecoveryFixture) observe(t *testing.T) {
	t.Helper()
	select {
	case f.ticks <- time.Time{}:
	case <-f.proc.exited:
		t.Fatal("unknown identity became exit evidence")
	case <-time.After(time.Second):
		t.Fatal("observer did not accept a tick")
	}
}

func (f *adoptedRecoveryFixture) assertRetained(t *testing.T) {
	t.Helper()
	if f.manager.sessions[f.session.id] != f.session || f.session.proc != f.proc || f.session.recoveryStopped {
		t.Fatal("unknown identity discarded cleanup custody")
	}
	if _, err := os.Stat(f.session.profile); err != nil {
		t.Fatal("unconfirmed process cleanup removed its working directory")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kills) != 0 || !f.alive[recoveryFakeBrowser] {
		t.Fatalf("unverified process was signalled: %v", f.kills)
	}
}

func (f *adoptedRecoveryFixture) finish(t *testing.T, wantKills int) {
	t.Helper()
	f.mu.Lock()
	f.faultPID, f.fault = 0, nil
	f.mu.Unlock()
	type result struct {
		confirmed bool
		err       error
	}
	finished := make(chan result, 1)
	go func() {
		ok, err := f.manager.StopRecoverySession(f.session.id)
		finished <- result{ok, err}
	}()
	for i := 0; i < wantKills; i++ {
		select {
		case <-f.killed:
		case <-time.After(time.Second):
			t.Fatal("verified process was not stopped")
		}
	}
	if wantKills > 0 {
		// A preceding observation can finish after the fake kill and already
		// confirm exit. That is valid evidence, not an unknown-identity failure.
		select {
		case f.ticks <- time.Time{}:
		case <-f.proc.exited:
		case <-time.After(time.Second):
			t.Fatal("observer did not confirm the stopped process")
		}
	}
	select {
	case r := <-finished:
		if !r.confirmed || r.err != nil {
			t.Fatalf("verified retry: %v %v", r.confirmed, r.err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("stop did not finish after verified exit")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kills) != wantKills {
		t.Fatalf("stops=%v want=%d", f.kills, wantKills)
	}
	if _, exists := f.manager.sessions[f.session.id]; exists {
		t.Fatal("confirmed stop retained session")
	}
}

func TestCobrowseRecoveryAdoptedIdentityErrorsRetainCustody(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		err  error
	}{
		{"browser-permission", recoveryFakeBrowser, os.ErrPermission},
		{"browser-io", recoveryFakeBrowser, io.ErrUnexpectedEOF},
		{"browser-parse", recoveryFakeBrowser, fmt.Errorf("synthetic malformed process identity")},
		{"companion-permission", recoveryFakeXvfb, os.ErrPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptedRecoveryFixture(t, true)
			f.mu.Lock()
			f.faultPID, f.fault = tc.pid, tc.err
			f.mu.Unlock()
			// A second unbuffered tick proves the previous observation returned
			// to its loop without closing exited, independently of scheduling.
			f.observe(t)
			f.observe(t)
			if f.manager.RecoveryState(f.session.id) == "ended" {
				t.Fatal("inspection error reported terminal state")
			}
			if yes, err := f.manager.StopRecoverySession(f.session.id); yes || err == nil {
				t.Fatalf("unknown identity stop confirmed: %v %v", yes, err)
			}
			f.assertRetained(t)
			f.finish(t, 2)
		})
	}
}

func TestCobrowseRecoveryAdoptedIdentityErrorAfterPreflightRefusesSignal(t *testing.T) {
	f := newAdoptedRecoveryFixture(t, false)
	f.mu.Lock()
	f.faultPID, f.fault, f.faultRead = recoveryFakeBrowser, os.ErrPermission, 2
	f.mu.Unlock()
	if yes, err := f.manager.StopRecoverySession(f.session.id); yes || err == nil {
		t.Fatalf("second identity read error confirmed: %v %v", yes, err)
	}
	f.assertRetained(t)
	f.finish(t, 1)
}

func TestCobrowseRecoveryAdoptedKnownExitNeverSignalsReusedPID(t *testing.T) {
	for _, state := range []string{"missing", "reused", "not-live"} {
		t.Run(state, func(t *testing.T) {
			f := newAdoptedRecoveryFixture(t, false)
			f.mu.Lock()
			switch state {
			case "missing":
				f.faultPID, f.fault = recoveryFakeBrowser, os.ErrNotExist
			case "reused":
				f.starts[recoveryFakeBrowser]++
			case "not-live":
				f.alive[recoveryFakeBrowser] = false
			}
			f.mu.Unlock()
			f.observe(t)
			select {
			case <-f.proc.exited:
			case <-time.After(time.Second):
				t.Fatal("verified exit not observed")
			}
			if f.manager.RecoveryState(f.session.id) != "ended" {
				t.Fatal("known exit lost")
			}
			// Keep the known-absence observation intact through stop.
			if yes, err := f.manager.StopRecoverySession(f.session.id); !yes || err != nil {
				t.Fatalf("verified ended session not cleaned: %v %v", yes, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.kills) != 0 {
				t.Fatalf("known absent/reused PID signalled: %v", f.kills)
			}
		})
	}
}
