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
