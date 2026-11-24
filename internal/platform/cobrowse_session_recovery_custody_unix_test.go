//go:build !windows

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// custodyProfile records what a recovery cleanup sealed, and when, without
// touching real keys. Persist checks the staging dir is already gone and can
// run a side effect (such as pinning part of the working dir) on success.
type custodyProfile struct {
	stagingDir           string
	persisted            []string
	closes               int
	stagingPresentAtSeal bool
	afterPersist         func()
}

func (p *custodyProfile) Materialize(string) error { return nil }
func (p *custodyProfile) Persist(dir string) error {
	p.persisted = append(p.persisted, dir)
	if _, err := os.Lstat(p.stagingDir); !os.IsNotExist(err) {
		p.stagingPresentAtSeal = true
	}
	if p.afterPersist != nil {
		p.afterPersist()
	}
	return nil
}
func (p *custodyProfile) Close() error { p.closes++; return nil }

// custodyFixture is a running persistent session with a staged file, a pending
// chooser watcher and a registry record, all in owned temp dirs. Its process is
// fake: stop closes the exit channel and counts calls.
type custodyFixture struct {
	m       *CobrowseSessionManager
	s       *cobrowseSession
	id      string
	profile string
	stage   string
	record  string
	enc     *custodyProfile
	stops   int
	watch   chan struct{}
}

func newCustodyFixture(t *testing.T, workSubdir string) *custodyFixture {
	t.Helper()
	prevA, prevD := sessionRemoveAttempts, sessionRemoveDelay
	sessionRemoveAttempts, sessionRemoveDelay = 1, time.Millisecond
	t.Cleanup(func() { sessionRemoveAttempts, sessionRemoveDelay = prevA, prevD })
	f := &custodyFixture{m: newCobrowseSessionManager(trustedBaseDir(t), 8), id: "cb-0123456789abcdef0123456789abcdef"}
	f.m.Recover()
	f.profile = filepath.Join(f.m.baseDir, f.id)
	f.stage = filepath.Join(f.profile, cobrowseStagingDirName)
	workDir := f.profile
	if workSubdir != "" {
		workDir = filepath.Join(f.profile, workSubdir)
	}
	for _, d := range []string{f.stage, workDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.stage, "a.pdf"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	regDir := t.TempDir()
	f.record = recordFile(regDir, f.id)
	if err := os.WriteFile(f.record, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.enc = &custodyProfile{stagingDir: f.stage}
	exited := make(chan struct{})
	f.watch = make(chan struct{})
	f.s = &cobrowseSession{
		id: f.id, profile: f.profile, workDir: workDir, regDir: regDir,
		state: SessionRunning, encProfile: f.enc, watchStop: f.watch,
		proc: &cobrowseProc{exited: exited, stop: func() error {
			f.stops++
			if f.stops == 1 {
				close(exited)
			}
			return nil
		}},
	}
	f.m.sessions[f.id] = f.s
	return f
}

// requireNonterminal asserts a failed attempt kept every piece of custody a
// retry needs: the map entry, process handle, unlocked profile and record.
func (f *custodyFixture) requireNonterminal(t *testing.T, confirmed bool, err error) {
	t.Helper()
	if confirmed || err == nil {
		t.Fatalf("uncertain cleanup confirmed: confirmed=%v err=%v", confirmed, err)
	}
	if f.m.sessions[f.id] != f.s || f.s.proc == nil || f.s.encProfile == nil {
		t.Fatal("unconfirmed cleanup released session custody")
	}
	if _, err := os.Stat(f.record); err != nil {
		t.Fatal("unconfirmed cleanup removed the registry record")
	}
}

func (f *custodyFixture) requireTerminal(t *testing.T, confirmed bool, err error) {
	t.Helper()
	if !confirmed || err != nil {
		t.Fatalf("cleanup retry failed: confirmed=%v err=%v", confirmed, err)
	}
	if _, ok := f.m.sessions[f.id]; ok {
		t.Fatal("confirmed cleanup kept the session")
	}
	if _, err := os.Lstat(f.profile); !os.IsNotExist(err) {
		t.Fatal("confirmed cleanup left the plaintext working dir")
	}
	if _, err := os.Stat(f.record); !os.IsNotExist(err) {
		t.Fatal("confirmed cleanup left the registry record")
	}
}

// Recovery runs teardown's first step: the chooser watcher stops, staging
// closes, staged files are gone before the seal, and the seal reads the
// container profile subdirectory, the same workDir teardown seals.
func TestCobrowseRecoveryCleanupUsesTeardownSealRules(t *testing.T) {
	installFakeHelpers(t, &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}})
	f := newCustodyFixture(t, containerProfileSubdir)
	confirmed, err := f.m.StopRecoverySession(f.id)
	f.requireTerminal(t, confirmed, err)
	select {
	case <-f.watch:
	default:
		t.Fatal("chooser watcher still running after recovery stop")
	}
	if !f.s.watchStopped || !f.s.stageClosed {
		t.Fatal("recovery left the chooser or staging open")
	}
	want := filepath.Join(f.profile, containerProfileSubdir)
	if len(f.enc.persisted) != 1 || f.enc.persisted[0] != want {
		t.Fatalf("sealed %v, want only %s", f.enc.persisted, want)
	}
	if f.enc.stagingPresentAtSeal {
		t.Fatal("sealed while staged files were still present")
	}
	if f.enc.closes != 1 || f.stops != 1 {
		t.Fatalf("closes=%d stops=%d", f.enc.closes, f.stops)
	}
}

// A browser helper still using the profile, or a process list that cannot be
// read, blocks the seal and leaves recovery nonterminal; a retry after the
// helper is gone seals once without stopping the browser again.
func TestCobrowseRecoveryProfileUserBlocksSeal(t *testing.T) {
	for _, mode := range []string{"survivor", "unlistable"} {
		t.Run(mode, func(t *testing.T) {
			h := &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}}
			installFakeHelpers(t, h)
			f := newCustodyFixture(t, "")
			helper := helperProc(4242, 7, h.euid, "chrome", "--type=renderer", "--user-data-dir="+f.profile)
			listErr := false
			switch mode {
			case "survivor":
				h.procs[helper.pid] = helper
				// The helper never dies: every kill is answered by the same process.
				h.onKill = func(int) {
					h.mu.Lock()
					h.procs[helper.pid] = helper
					h.mu.Unlock()
				}
			case "unlistable":
				listErr = true
				prev := listReapProcs
				listReapProcs = func() ([]reapProc, error) {
					if listErr {
						return nil, errors.New("synthetic process list failure")
					}
					return prev()
				}
			}
			confirmed, err := f.m.StopRecoverySession(f.id)
			f.requireNonterminal(t, confirmed, err)
			if len(f.enc.persisted) != 0 || f.enc.closes != 0 {
				t.Fatalf("sealed or closed with an unverified profile user: persist=%d close=%d", len(f.enc.persisted), f.enc.closes)
			}
			if _, err := os.Stat(f.profile); err != nil {
				t.Fatal("plaintext removed before sealing")
			}
			h.mu.Lock()
			h.onKill = nil
			delete(h.procs, helper.pid)
			h.mu.Unlock()
			listErr = false
			confirmed, err = f.m.StopRecoverySession(f.id)
			f.requireTerminal(t, confirmed, err)
			if len(f.enc.persisted) != 1 || f.enc.closes != 1 || f.stops != 1 {
				t.Fatalf("retry stages: persist=%d close=%d stops=%d", len(f.enc.persisted), f.enc.closes, f.stops)
			}
		})
	}
}

// A staging dir that cannot be removed is never sealed, and the recovery
// stays nonterminal until it can be.
func TestCobrowseRecoveryUnremovableStagingNeverSeals(t *testing.T) {
	installFakeHelpers(t, &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}})
	f := newCustodyFixture(t, "")
	pinDirUnremovable(t, f.stage)
	confirmed, err := f.m.StopRecoverySession(f.id)
	f.requireNonterminal(t, confirmed, err)
	if len(f.enc.persisted) != 0 || f.enc.closes != 0 {
		t.Fatal("sealed or closed while staged files could not be removed")
	}
	if err := os.Chmod(filepath.Join(f.stage, "pinned"), 0o700); err != nil {
		t.Fatal(err)
	}
	confirmed, err = f.m.StopRecoverySession(f.id)
	f.requireTerminal(t, confirmed, err)
	if len(f.enc.persisted) != 1 || f.enc.stagingPresentAtSeal || f.stops != 1 {
		t.Fatalf("retry: persist=%v stagingPresent=%v stops=%d", f.enc.persisted, f.enc.stagingPresentAtSeal, f.stops)
	}
}

// After a successful seal, a working dir that cannot be removed, or a profile
// user that cannot be checked during removal, stays nonterminal; the retry
// does not seal or close again.
func TestCobrowseRecoveryPostSealRemovalFailureIsRetryable(t *testing.T) {
	for _, mode := range []string{"unremovable", "unverified-reap"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "unremovable" && os.Geteuid() == 0 {
				t.Skip("root can remove read-only directories")
			}
			installFakeHelpers(t, &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}})
			f := newCustodyFixture(t, "")
			failList := false
			prev := listReapProcs
			listReapProcs = func() ([]reapProc, error) {
				if failList {
					return nil, errors.New("synthetic process list failure")
				}
				return prev()
			}
			pinned := filepath.Join(f.profile, "Default")
			f.enc.afterPersist = func() {
				if mode == "unremovable" {
					pinDirUnremovable(t, pinned)
				} else {
					failList = true
				}
			}
			confirmed, err := f.m.StopRecoverySession(f.id)
			f.requireNonterminal(t, confirmed, err)
			if len(f.enc.persisted) != 1 || f.enc.closes != 1 {
				t.Fatalf("first attempt: persist=%d close=%d", len(f.enc.persisted), f.enc.closes)
			}
			f.enc.afterPersist = nil
			failList = false
			if mode == "unremovable" {
				if err := os.Chmod(filepath.Join(pinned, "pinned"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			confirmed, err = f.m.StopRecoverySession(f.id)
			f.requireTerminal(t, confirmed, err)
			if len(f.enc.persisted) != 1 || f.enc.closes != 1 || f.stops != 1 {
				t.Fatalf("retry repeated stages: persist=%d close=%d stops=%d", len(f.enc.persisted), f.enc.closes, f.stops)
			}
		})
	}
}

// A start refused before a session slot is taken carries ErrSessionNotLaunched
// (no browser can exist); a duplicate handle does not, because a browser
// already holds it and recovery must keep custody of that one.
func TestCobrowseRecoveryPreSlotRefusalsAreNotLaunched(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 1)
	held := "cb-0123456789abcdef0123456789abcdef"
	if _, err := m.StartRecoverySessionWithID(held, "", nil); err != nil {
		t.Fatal(err)
	}
	other := "cb-fedcba9876543210fedcba9876543210"
	for name, start := range map[string]func() error{
		"session-limit": func() error { _, err := m.StartRecoverySessionWithID(other, "", nil); return err },
		"bad-url":       func() error { _, err := m.StartRecoverySessionWithID(other, "file:///etc/passwd", nil); return err },
		"bad-handle":    func() error { _, err := m.StartRecoverySessionWithID("cb-short", "", nil); return err },
	} {
		if err := start(); !errors.Is(err, ErrSessionNotLaunched) {
			t.Fatalf("%s: %v is not a not-launched refusal", name, err)
		}
	}
	if err := func() error { _, err := m.StartRecoverySessionWithID(held, "", nil); return err }(); err == nil || errors.Is(err, ErrSessionNotLaunched) {
		t.Fatalf("duplicate handle reported as not launched: %v", err)
	}
	if f.n != 1 {
		t.Fatalf("a refused start launched a browser: %d launches", f.n)
	}
}

// Every legacy stop path routes a recovery handle through recovery custody: a
// confirmed stop leaves evidence the journal terminalizes on, and a stop that
// cannot be confirmed keeps the session (nonterminal) for a retry.
func TestCobrowseRecoveryLegacyStopPathsKeepCustody(t *testing.T) {
	paths := map[string]func(m *CobrowseSessionManager, id string) bool{
		"stop": func(m *CobrowseSessionManager, id string) bool { return m.Stop(id) == nil },
		"stop-all": func(m *CobrowseSessionManager, id string) bool {
			return m.StopAll() == nil
		},
		// The managed worker's shutdown branch (not its StopAll fallback): an
		// unscoped recovery session is stopped under custody, not left running.
		"shutdown": func(m *CobrowseSessionManager, id string) bool {
			prevDetached, prevAdopt, prevResolver := detachedWorker.Load(), adoptionSupported, m.useResolver
			detachedWorker.Store(true)
			adoptionSupported, m.useResolver = true, true
			defer func() {
				detachedWorker.Store(prevDetached)
				adoptionSupported, m.useResolver = prevAdopt, prevResolver
			}()
			return m.ShutdownSessions() == nil
		},
		"public-only": func(m *CobrowseSessionManager, id string) bool { return m.stopUnguarded(id) },
		"guard-failure": func(m *CobrowseSessionManager, id string) bool {
			failed := make(chan struct{})
			close(failed)
			m.watchGuard(id, failed, nil)
			_, present := m.sessions[id]
			return !present
		},
	}
	for name, stop := range paths {
		t.Run(name, func(t *testing.T) {
			installFakeHelpers(t, &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}})
			// Unconfirmed: the staging dir sits under a regular file, so the
			// seal and removal cannot be confirmed (root-independent).
			f := newCustodyFixture(t, "")
			blocker := filepath.Join(f.m.baseDir, "blocked")
			if err := os.WriteFile(blocker, []byte("owned fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			good := f.s.profile
			f.s.profile = filepath.Join(blocker, "profile")
			if stop(f.m, f.id) {
				t.Fatal("unconfirmed custody stop reported success")
			}
			f.requireNonterminal(t, false, errors.New("unconfirmed"))
			if f.m.RecoveryState(f.id) == "unknown" || len(f.enc.persisted) != 0 {
				t.Fatal("unconfirmed stop lost the session or sealed")
			}
			// Confirmed on the same path once cleanup can finish.
			f.s.profile = good
			if !stop(f.m, f.id) {
				t.Fatal("custody stop not confirmed after cleanup became possible")
			}
			f.requireTerminal(t, true, nil)
			if f.m.RecoveryState(f.id) != "ended" {
				t.Fatal("confirmed stop left no terminal evidence")
			}
			if yes, err := f.m.StopRecoverySession(f.id); !yes || err != nil {
				t.Fatalf("journal stop after a legacy stop: %v %v", yes, err)
			}
			if f.stops != 1 || len(f.enc.persisted) != 1 || f.enc.closes != 1 {
				t.Fatalf("stages: stops=%d persist=%d close=%d", f.stops, len(f.enc.persisted), f.enc.closes)
			}
		})
	}
}

// Teardown never seals or closes again after recovery cleanup already did.
func TestTeardownSkipsStagesRecoveryCompleted(t *testing.T) {
	installFakeHelpers(t, &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}})
	f := newCustodyFixture(t, "")
	f.s.recoveryStopped, f.s.recoveryPersisted, f.s.recoveryClosed = true, true, true
	if err := f.s.teardown(); err != nil {
		t.Fatal(err)
	}
	if len(f.enc.persisted) != 0 || f.enc.closes != 0 {
		t.Fatalf("teardown repeated recovery stages: persist=%d close=%d", len(f.enc.persisted), f.enc.closes)
	}
}

// Confirmed-stop evidence never outlives a new session under the same handle:
// a later start that fails after taking its slot, with a helper still using the
// dir, must read unknown, not ended.
func TestCobrowseRecoveryEvidenceClearedByNewStart(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	prevA, prevD := sessionRemoveAttempts, sessionRemoveDelay
	sessionRemoveAttempts, sessionRemoveDelay = 1, time.Millisecond
	t.Cleanup(func() { sessionRemoveAttempts, sessionRemoveDelay = prevA, prevD })
	h := &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}}
	installFakeHelpers(t, h)
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	id := "cb-0123456789abcdef0123456789abcdef"
	helper := helperProc(4243, 9, h.euid, "chrome", "--type=renderer", "--user-data-dir="+filepath.Join(m.baseDir, id))
	h.procs[helper.pid] = helper
	h.onKill = func(int) {
		h.mu.Lock()
		h.procs[helper.pid] = helper
		h.mu.Unlock()
	}
	m.mu.Lock()
	m.markRecoveryDoneLocked(id)
	m.mu.Unlock()
	if m.RecoveryState(id) != "ended" {
		t.Fatal("fixture evidence missing")
	}
	prev := launchCobrowseProc
	launchCobrowseProc = func(string, string, LaunchOptions) (*cobrowseProc, error) {
		return nil, errors.New("synthetic launch failure after the slot was taken")
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
	if _, err := m.StartRecoverySessionWithID(id, "", nil); err == nil || errors.Is(err, ErrSessionNotLaunched) {
		t.Fatalf("post-slot launch failure: %v", err)
	}
	if m.RecoveryState(id) != "unknown" {
		t.Fatal("stale evidence made a failed relaunch look ended")
	}
	if yes, err := m.StopRecoverySession(id); yes || err != nil {
		t.Fatalf("stale evidence confirmed a stop: %v %v", yes, err)
	}
}

// A launch that fails after taking its slot is terminal only once no process
// still names the session dir and the dir is gone. Until then the session
// stays under custody, and a later custody stop retries the same check.
func TestCobrowseRecoveryPostSlotLaunchFailure(t *testing.T) {
	for _, mode := range []string{"confirmed", "survivor-then-retry"} {
		t.Run(mode, func(t *testing.T) {
			prevA, prevD := sessionRemoveAttempts, sessionRemoveDelay
			sessionRemoveAttempts, sessionRemoveDelay = 1, time.Millisecond
			t.Cleanup(func() { sessionRemoveAttempts, sessionRemoveDelay = prevA, prevD })
			h := &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}}
			installFakeHelpers(t, h)
			m := newCobrowseSessionManager(trustedBaseDir(t), 2)
			id := "cb-0123456789abcdef0123456789abcdef"
			profile := filepath.Join(m.baseDir, id)
			helper := helperProc(4242, 7, h.euid, "chrome", "--type=gpu-process", "--user-data-dir="+profile)
			prev := launchCobrowseProc
			launchCobrowseProc = func(string, string, LaunchOptions) (*cobrowseProc, error) {
				if mode != "confirmed" {
					// A helper outlives the failed launch and never dies.
					h.mu.Lock()
					h.procs[helper.pid] = helper
					h.onKill = func(int) {
						h.mu.Lock()
						h.procs[helper.pid] = helper
						h.mu.Unlock()
					}
					h.mu.Unlock()
				}
				return nil, errors.New("synthetic launch failure after the slot was taken")
			}
			t.Cleanup(func() { launchCobrowseProc = prev })
			if _, err := m.StartRecoverySessionWithID(id, "", nil); err == nil || errors.Is(err, ErrSessionNotLaunched) {
				t.Fatalf("post-slot failure: %v", err)
			}
			if mode == "confirmed" {
				if m.RecoveryState(id) != "ended" || m.RecoveryPresent(id) {
					t.Fatal("a confirmed failed launch left no terminal evidence")
				}
				if yes, err := m.StopRecoverySession(id); !yes || err != nil {
					t.Fatalf("journal stop: %v %v", yes, err)
				}
				return
			}
			if m.RecoveryState(id) != "unknown" || !m.RecoveryPresent(id) {
				t.Fatal("an unconfirmed failed launch was released or marked ended")
			}
			if yes, err := m.StopRecoverySession(id); yes || err != nil {
				t.Fatalf("unconfirmed retry stopped: %v %v", yes, err)
			}
			if err := m.Stop(id); !errors.Is(err, errRecoveryStopUnconfirmed) {
				t.Fatalf("legacy stop of an unconfirmed failed launch: %v", err)
			}
			h.mu.Lock()
			h.onKill = nil
			delete(h.procs, helper.pid)
			h.mu.Unlock()
			if yes, err := m.StopRecoverySession(id); !yes || err != nil {
				t.Fatalf("retry after the helper left: %v %v", yes, err)
			}
			if m.RecoveryPresent(id) || m.RecoveryState(id) != "ended" {
				t.Fatal("confirmed retry kept custody or left no evidence")
			}
			if _, err := os.Lstat(profile); !os.IsNotExist(err) {
				t.Fatal("session dir left behind")
			}
		})
	}
}

// A legacy stop racing StopAll or the managed shutdown never reports success
// for a recovery session the shutdown path holds but has not resolved, and a
// session the managed shutdown leaves running stays unconfirmed.
func TestCobrowseRecoveryLegacyStopRacesShutdown(t *testing.T) {
	for _, mode := range []string{"stop-all", "shutdown-stop", "shutdown-keep"} {
		t.Run(mode, func(t *testing.T) {
			installFakeHelpers(t, &fakeHelpers{euid: os.Geteuid(), procs: map[int]reapProc{}})
			f := newCustodyFixture(t, "")
			f.s.encProfile = nil // throwaway, so the managed shutdown may keep it
			exited := make(chan struct{})
			entered, release := make(chan struct{}), make(chan struct{})
			f.s.proc = &cobrowseProc{exited: exited, stop: func() error {
				close(entered)
				<-release
				close(exited)
				return nil
			}}
			if mode == "shutdown-keep" {
				f.s.proc.browserScope = "cb-fixture.scope"
			}
			done := make(chan error, 1)
			go func() {
				if mode == "stop-all" {
					done <- f.m.StopAll()
					return
				}
				prevDetached, prevAdopt := detachedWorker.Load(), adoptionSupported
				detachedWorker.Store(true)
				adoptionSupported = true
				f.m.mu.Lock()
				f.m.useResolver = true
				f.m.mu.Unlock()
				err := f.m.ShutdownSessions()
				detachedWorker.Store(prevDetached)
				adoptionSupported = prevAdopt
				done <- err
			}()
			if mode == "shutdown-keep" {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			} else {
				<-entered
			}
			// The shutdown path holds the session: a stop must not succeed.
			if err := f.m.Stop(f.id); !errors.Is(err, errRecoveryStopUnconfirmed) {
				t.Fatalf("legacy stop during %s: %v", mode, err)
			}
			if yes, err := f.m.StopRecoverySession(f.id); yes || err != nil {
				t.Fatalf("journal stop during %s: %v %v", mode, yes, err)
			}
			if !f.m.RecoveryPresent(f.id) {
				t.Fatal("held session not visible to a no-record stop")
			}
			if mode == "shutdown-keep" {
				return
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := f.m.Stop(f.id); err != nil {
				t.Fatalf("stop after the shutdown confirmed: %v", err)
			}
			if f.m.RecoveryState(f.id) != "ended" {
				t.Fatal("confirmed shutdown stop left no evidence")
			}
		})
	}
}
