package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const suspendStartTestID = "cb-0123456789abcdef"

type suspendStartRunner struct{ sweep func() }

func (s suspendStartRunner) Name() string                   { return "fake" }
func (s suspendStartRunner) Discover() (BrowserInfo, error) { return BrowserInfo{}, nil }
func (s suspendStartRunner) PrepareBaseDir(string) error    { return nil }
func (s suspendStartRunner) Launch(string, string, LaunchOptions) (*cobrowseProc, error) {
	return nil, errors.New("fake runner has no browser")
}
func (s suspendStartRunner) SweepOrphans(string) {
	if s.sweep != nil {
		s.sweep()
	}
}

func suspendStartFixture(t *testing.T) (*CobrowseSessionManager, string) {
	t.Helper()
	base := trustedBaseDir(t)
	m := newCobrowseSessionManager(base, 2)
	m.useResolver = true
	dir := base + registryDirSuffix
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatal(err)
	}
	previous := registryDirResolver
	registryDirResolver = func() string { return dir }
	t.Cleanup(func() { registryDirResolver = previous })
	return m, dir
}

func writeSuspendStartRecord(t *testing.T, dir string, rec sessionRecord) []byte {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, suspendStartTestID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSuspendStartPublicRefusesRetainedIdentityBeforeEffects(t *testing.T) {
	for _, kind := range []string{"suspended", "suspending", "resuming", "live-retained", "expired-retained", "tombstone", "unknown", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			m, dir := suspendStartFixture(t)
			rec := sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: kind, Generation: 7}
			switch kind {
			case "live-retained":
				rec.State, rec.Suspension = RecordLive, &suspendMetadata{}
			case "expired-retained":
				rec.State, rec.Suspension = RecordExpired, &suspendMetadata{}
			case "tombstone":
				rec.State, rec.EndedReason = RecordExpired, "suspend_expired"
			}
			before := writeSuspendStartRecord(t, dir, rec)
			if kind == "invalid" {
				before = []byte("private-capture-sentinel invalid JSON")
				if err := os.WriteFile(filepath.Join(dir, suspendStartTestID+".json"), before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			launches := 0
			previous := launchCobrowseProc
			launchCobrowseProc = func(string, string, LaunchOptions) (*cobrowseProc, error) {
				launches++
				return nil, errors.New("synthetic launch must not run")
			}
			t.Cleanup(func() { launchCobrowseProc = previous })
			_, err := m.StartSessionWithID(suspendStartTestID, "about:blank", nil)
			if err == nil || launches != 0 || len(m.sessions) != 0 || strings.Contains(err.Error(), "private-capture-sentinel") {
				t.Fatal("legacy start reused a retained identity, reserved a slot, or exposed metadata", err)
			}
			// The first refusal must precede startup recovery/sweep, not only launch.
			sweepUnused := false
			m.sweepOnce.Do(func() { sweepUnused = true })
			if !sweepUnused {
				t.Fatal("retained start ran startup sweep before refusal")
			}
			after, readErr := os.ReadFile(filepath.Join(dir, suspendStartTestID+".json"))
			if readErr != nil || string(before) != string(after) {
				t.Fatal("refused start changed retained metadata")
			}
			if _, err := os.Stat(filepath.Join(m.baseDir, suspendStartTestID)); !os.IsNotExist(err) {
				t.Fatal("refused start created a profile")
			}
			if _, err := os.Stat(filepath.Join(dir, suspendManifestDirName)); !os.IsNotExist(err) {
				t.Fatal("reservation opened/created capture storage")
			}
		})
	}
}

func TestSuspendStartLegacyAndLocalCompatibility(t *testing.T) {
	for _, state := range []string{"missing", RecordLive, RecordExpired} {
		t.Run(state, func(t *testing.T) {
			m, dir := suspendStartFixture(t)
			var before []byte
			if state != "missing" {
				before = writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: state})
			}
			calls := 0
			err := m.withNewSessionRegistryFence(suspendPagesContext(t), suspendStartTestID, func(ctx context.Context, got string) error {
				calls++
				if got != dir || ctx.Err() != nil {
					t.Fatal("reservation lost its pinned registry or budget")
				}
				short, cancel := context.WithTimeout(ctx, time.Millisecond)
				defer cancel()
				if release, err := acquireBrowserRegistryGate(short, dir); err == nil {
					release()
					t.Fatal("slot reservation lost registry exclusion")
				}
				return nil
			})
			if err != nil || calls != 1 {
				t.Fatal("legacy start compatibility changed", err)
			}
			if before != nil {
				after, _ := os.ReadFile(filepath.Join(dir, suspendStartTestID+".json"))
				if string(after) != string(before) {
					t.Fatal("read-only fence rewrote a legacy record")
				}
			}
		})
	}
	t.Run("local-mode", func(t *testing.T) {
		m, dir := suspendStartFixture(t)
		writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: RecordSuspended})
		m.useResolver = false
		registryDirResolver = func() string { t.Fatal("local start inspected node registry"); return dir }
		calls := 0
		if err := m.withNewSessionRegistryFence(suspendPagesContext(t), suspendStartTestID, func(_ context.Context, got string) error {
			calls++
			if got != "" {
				t.Fatal("local reservation acquired a node registry")
			}
			return nil
		}); err != nil || calls != 1 {
			t.Fatal(err)
		}
	})
	t.Run("absent-registry", func(t *testing.T) {
		m, dir := suspendStartFixture(t)
		missing := filepath.Join(dir, "unused")
		registryDirResolver = func() string { return missing }
		if err := m.withNewSessionRegistryFence(suspendPagesContext(t), suspendStartTestID, func(_ context.Context, got string) error {
			if got != missing {
				t.Fatal("missing registry binding changed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatal("read-only fence created a registry")
		}
	})
	t.Run("generated-fallback-shape", func(t *testing.T) {
		m, _ := suspendStartFixture(t)
		calls := 0
		if err := m.withNewSessionRegistryFence(suspendPagesContext(t), "cb-1234567890123456789", func(context.Context, string) error {
			calls++
			return nil
		}); err != nil || calls != 1 {
			t.Fatal("internal generated-id fallback shape was rejected", err)
		}
	})
}

func TestSuspendStartPublicFreshReservation(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "managed", true: "local"}[local], func(t *testing.T) {
			m, dir := suspendStartFixture(t)
			m.useResolver = !local
			m.sweepOnce.Do(func() {}) // This test concerns start, not startup recovery.
			previousRunner, previousLaunch := activeBrowserRunner, launchCobrowseProc
			activeBrowserRunner = suspendStartRunner{}
			t.Cleanup(func() { activeBrowserRunner, launchCobrowseProc = previousRunner, previousLaunch })
			launches, stops := 0, 0
			launchCobrowseProc = func(string, string, LaunchOptions) (*cobrowseProc, error) {
				launches++
				return &cobrowseProc{browserID: "fake-browser", exited: make(chan struct{}), stop: func() error { stops++; return nil }}, nil
			}
			got, err := m.StartSessionWithID(suspendStartTestID, "about:blank", nil)
			if err != nil || got.State != SessionRunning || launches != 1 {
				t.Fatal("fresh start no longer launches", err)
			}
			s := m.sessions[suspendStartTestID]
			expectedDir := dir
			if local {
				expectedDir = ""
			}
			if s == nil || s.regDir != expectedDir || s.generation != 1 {
				t.Fatal("slot lost its registry or generation")
			}
			if err := m.Stop(suspendStartTestID); err != nil || stops != 1 {
				t.Fatal("fresh start no longer stops normally", err)
			}
		})
	}
}

func TestSuspendStartRechecksAfterStartupSweep(t *testing.T) {
	m, dir := suspendStartFixture(t)
	previousRunner, previousLaunch := activeBrowserRunner, launchCobrowseProc
	t.Cleanup(func() { activeBrowserRunner, launchCobrowseProc = previousRunner, previousLaunch })
	var protected []byte
	activeBrowserRunner = suspendStartRunner{sweep: func() {
		protected = writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: "suspending"})
	}}
	launches := 0
	launchCobrowseProc = func(string, string, LaunchOptions) (*cobrowseProc, error) {
		launches++
		return nil, errors.New("fake launch after preflight must not run")
	}
	_, err := m.StartSessionWithID(suspendStartTestID, "about:blank", nil)
	if err == nil || launches != 0 || len(m.sessions) != 0 || protected == nil {
		t.Fatal("start trusted the pre-sweep snapshot instead of rechecking", err)
	}
	after, readErr := os.ReadFile(filepath.Join(dir, suspendStartTestID+".json"))
	if readErr != nil || string(after) != string(protected) {
		t.Fatal("start replaced metadata published during startup")
	}
}

func TestSuspendStartWaitRechecksPublishedState(t *testing.T) {
	m, dir := suspendStartFixture(t)
	release, err := acquireBrowserRegistryGate(suspendPagesContext(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	done := make(chan error, 1)
	go func() {
		done <- m.withNewSessionRegistryFence(suspendPagesContext(t), suspendStartTestID, func(context.Context, string) error {
			return errors.New("reservation must not run after protected publication")
		})
	}()
	select {
	case <-done:
		t.Fatal("start did not wait for registry exclusion")
	case <-time.After(20 * time.Millisecond):
	}
	writeSuspendStartRecord(t, dir, sessionRecord{Version: sessionRecordVersion, ID: suspendStartTestID, State: "suspending"})
	release()
	released = true
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "reservation must not") {
			t.Fatal("start used a pre-wait registry snapshot", err)
		}
	case <-time.After(time.Second):
		t.Fatal("start did not release its wait")
	}
}

func TestSuspendStartAdmissionAndBudget(t *testing.T) {
	for _, kind := range []string{"nil-context", "unbounded", "cancelled", "id", "version", "wrong-id", "oversize", "null", "directory"} {
		t.Run(kind, func(t *testing.T) {
			m, dir := suspendStartFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			id := suspendStartTestID
			rec := sessionRecord{Version: sessionRecordVersion, ID: id, State: RecordLive}
			switch kind {
			case "nil-context":
				ctx = nil
			case "unbounded":
				ctx = context.Background()
			case "cancelled":
				cancel()
			case "id":
				id = "cb-not-reserved"
			case "version":
				rec.Version++
			case "wrong-id":
				rec.ID = "cb-aaaaaaaaaaaaaaaa"
			}
			writeSuspendStartRecord(t, dir, rec)
			name := filepath.Join(dir, suspendStartTestID+".json")
			switch kind {
			case "oversize":
				if err := os.WriteFile(name, []byte(strings.Repeat("x", maxSuspendRegistryRecordBytes+1)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "null":
				if err := os.WriteFile(name, []byte("null"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			if err := m.withNewSessionRegistryFence(ctx, id, func(context.Context, string) error { calls++; return nil }); err == nil || calls != 0 {
				t.Fatal("invalid admission reached reservation")
			}
		})
	}
	t.Run("wait-cancellation-and-retry", func(t *testing.T) {
		m, dir := suspendStartFixture(t)
		release, err := acquireBrowserRegistryGate(suspendPagesContext(t), dir)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		calls := 0
		reserve := func(context.Context, string) error { calls++; return nil }
		err = m.withNewSessionRegistryFence(ctx, suspendStartTestID, reserve)
		release()
		if err == nil || calls != 0 {
			t.Fatal("cancelled gate wait reserved a session")
		}
		if err := m.withNewSessionRegistryFence(suspendPagesContext(t), suspendStartTestID, reserve); err != nil || calls != 1 {
			t.Fatal("cancelled wait poisoned later reservation", err)
		}
	})
	t.Run("long-parent-capped", func(t *testing.T) {
		m, _ := suspendStartFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if err := m.withNewSessionRegistryFence(ctx, suspendStartTestID, func(child context.Context, _ string) error {
			deadline, finite := child.Deadline()
			if !finite || time.Until(deadline) > suspendStartFenceLimit {
				t.Fatal("reservation lacks its finite ceiling")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
