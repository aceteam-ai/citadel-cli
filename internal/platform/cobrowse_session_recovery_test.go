package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/cobrowserecovery"
)

func TestCobrowseRecoveryPreassignedIDAndDuplicateRefusal(t *testing.T) {
	f := &fakeLauncher{}
	f.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	id := "cb-c8409b9afcfb4677b9f2813bc0110b91"
	// Empty URL avoids native/CDP navigation settling; the launcher is fake.
	st, err := m.StartRecoverySessionWithID(id, "", nil)
	if err != nil || st.ID != id {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err = m.StartRecoverySessionWithID(id, "", nil); err == nil {
		t.Fatal("duplicate session replaced")
	}
	if f.n != 1 || filepath.Base(f.profiles[0]) != id {
		t.Fatal("preassigned identity lost")
	}
	for _, bad := range []string{"../outside", "cb-xyz", "cb-C8409B9AFCFB4677B9F2813BC0110B91", NewCobrowseSessionID()} {
		if _, err = m.StartRecoverySessionWithID(bad, "", nil); err == nil {
			t.Fatal("unsafe handle accepted")
		}
	}
	if f.n != 1 {
		t.Fatal("invalid ID launched")
	}
	if _, err := m.StartSessionWithID(id, "", nil); err == nil {
		t.Fatal("recovery handle accepted by local reserved-ID entrypoint")
	}
	_ = m.Stop(id)
}
func TestCobrowseRecoveryExitEvidenceAndSingleStop(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	id := "cb-owned"
	exited := make(chan struct{})
	stops := 0
	profile := filepath.Join(m.baseDir, id)
	os.MkdirAll(profile, 0700)
	m.sessions[id] = &cobrowseSession{id: id, profile: profile, state: SessionRunning, proc: &cobrowseProc{exited: exited, stop: func() error { stops++; close(exited); return nil }}}
	if m.RecoveryState(id) != "running" {
		t.Fatal("live session missing")
	}
	confirmed, err := m.StopRecoverySession(id)
	if err != nil || !confirmed || stops != 1 {
		t.Fatalf("confirmed=%v err=%v calls=%d", confirmed, err, stops)
	}
	if m.RecoveryState(id) != "unknown" {
		t.Fatal("absence falsely terminal")
	}
	if yes, err := m.StopRecoverySession("missing"); err != nil || yes {
		t.Fatal("missing session falsely stopped")
	}
}
func TestCobrowseRecoveryExitedAndLaunchingEvidence(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	closed := make(chan struct{})
	close(closed)
	m.sessions["ended"] = &cobrowseSession{state: SessionRunning, proc: &cobrowseProc{exited: closed}}
	m.sessions["launching"] = &cobrowseSession{state: SessionLaunching}
	if m.RecoveryState("ended") != "ended" || m.RecoveryState("launching") != "unknown" {
		t.Fatal("invalid lifecycle evidence")
	}
	if confirmed, err := m.StopRecoverySession("launching"); err != nil || confirmed {
		t.Fatal("in-flight launch falsely stopped")
	}
}

func TestCobrowseRecoveryFailedStopRetainsProcessForRetry(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	exited := make(chan struct{})
	calls := 0
	m.sessions["retry"] = &cobrowseSession{id: "retry", state: SessionRunning, proc: &cobrowseProc{exited: exited, stop: func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("not stopped")
		}
		close(exited)
		return nil
	}}}
	if confirmed, err := m.StopRecoverySession("retry"); err == nil || confirmed {
		t.Fatal("failed stop confirmed")
	}
	if m.RecoveryState("retry") != "running" {
		t.Fatal("failed stop lost process custody")
	}
	if confirmed, err := m.StopRecoverySession("retry"); err != nil || !confirmed || calls != 2 {
		t.Fatalf("retry: %v %v %d", confirmed, err, calls)
	}
}

func TestCobrowseRecoveryStopNeedsObservedExit(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	exited := make(chan struct{})
	calls := 0
	m.sessions["wait"] = &cobrowseSession{id: "wait", state: SessionRunning, proc: &cobrowseProc{exited: exited, stop: func() error { calls++; return nil }}}
	if confirmed, err := m.StopRecoverySession("wait"); err == nil || confirmed {
		t.Fatal("stop return without exit falsely confirmed")
	}
	if m.RecoveryState("wait") != "running" {
		t.Fatal("lost unconfirmed process")
	}
	close(exited)
	if confirmed, err := m.StopRecoverySession("wait"); err != nil || !confirmed || calls != 2 {
		t.Fatalf("late exit not recoverable: %v %v %d", confirmed, err, calls)
	}
}

// retryRecoveryProfile fails a cleanup stage once without touching real keys.
type retryRecoveryProfile struct {
	persistCalls, closeCalls int
	failPersist, failClose   bool
	sourceSurvived           bool
}

func (p *retryRecoveryProfile) Materialize(string) error { return nil }
func (p *retryRecoveryProfile) Persist(dir string) error {
	p.persistCalls++
	if b, err := os.ReadFile(filepath.Join(dir, "Cookies")); err == nil && string(b) == "synthetic" {
		p.sourceSurvived = true
	}
	if p.failPersist && p.persistCalls == 1 {
		return fmt.Errorf("synthetic persistence failure")
	}
	return nil
}
func (p *retryRecoveryProfile) Close() error {
	p.closeCalls++
	if p.failClose && p.closeCalls == 1 {
		return fmt.Errorf("synthetic close failure")
	}
	return nil
}

func TestCobrowseRecoveryProfilePersistenceRetainsCustodyForRetry(t *testing.T) {
	for _, stage := range []string{"persist", "close"} {
		t.Run(stage, func(t *testing.T) {
			m := newCobrowseSessionManager(trustedBaseDir(t), 8)
			m.Recover()
			id := "cb-retry-profile"
			profile := filepath.Join(m.baseDir, id)
			if err := os.Mkdir(profile, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(profile, "Cookies"), []byte("synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
			p := &retryRecoveryProfile{failPersist: stage == "persist", failClose: stage == "close"}
			exited := make(chan struct{})
			stops := 0
			s := &cobrowseSession{id: id, profile: profile, state: SessionRunning, encProfile: p, proc: &cobrowseProc{exited: exited, stop: func() error {
				stops++
				if stops == 1 {
					close(exited)
				}
				return nil
			}}}
			m.sessions[id] = s
			if yes, err := m.StopRecoverySession(id); yes || err == nil {
				t.Fatal("cleanup failure falsely confirmed")
			}
			if m.sessions[id] != s || s.proc == nil || s.encProfile != p {
				t.Fatal("unconfirmed cleanup lost retry custody")
			}
			if _, err := os.Stat(filepath.Join(profile, "Cookies")); err != nil {
				t.Fatal("plaintext removed before sealing succeeded")
			}
			if stage == "persist" && p.closeCalls != 0 {
				t.Fatal("closed the only sealing handle after failed persistence")
			}
			if yes, err := m.StopRecoverySession(id); !yes || err != nil {
				t.Fatalf("retry failed: %v %v", yes, err)
			}
			wantPersist := 1
			if stage == "persist" {
				wantPersist = 2
			}
			if p.persistCalls != wantPersist || !p.sourceSurvived || stops != 1 {
				t.Fatalf("bad retry stages: persist=%d close=%d stops=%d", p.persistCalls, p.closeCalls, stops)
			}
			if _, ok := m.sessions[id]; ok {
				t.Fatal("confirmed cleanup kept active custody")
			}
			if _, err := os.Stat(profile); !os.IsNotExist(err) {
				t.Fatal("confirmed cleanup left plaintext directory")
			}
		})
	}
}

func TestCobrowseRecoveryPlaintextRemovalFailureIsRetryable(t *testing.T) {
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	id := "cb-remove-retry"
	// An existing regular file used as a parent deterministically produces ENOTDIR
	// on removal, even for a privileged test process. It needs no chmod/root skip.
	blocker := filepath.Join(m.baseDir, "blocked")
	if err := os.WriteFile(blocker, []byte("owned fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(blocker, "profile")
	exited := make(chan struct{})
	stops := 0
	p := &retryRecoveryProfile{}
	s := &cobrowseSession{id: id, profile: profile, state: SessionRunning, encProfile: p, proc: &cobrowseProc{exited: exited, stop: func() error {
		stops++
		if stops == 1 {
			close(exited)
		}
		return nil
	}}}
	m.sessions[id] = s
	if yes, err := m.StopRecoverySession(id); yes || err == nil {
		t.Fatal("unremoved working path falsely confirmed")
	}
	if m.sessions[id] != s || s.proc == nil {
		t.Fatal("removal failure lost cleanup custody")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "Cookies"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if yes, err := m.StopRecoverySession(id); !yes || err != nil {
		t.Fatalf("removal retry failed: %v %v", yes, err)
	}
	if stops != 1 || p.persistCalls != 1 || p.closeCalls != 1 {
		t.Fatal("retry repeated completed cleanup stages")
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("plaintext still exists")
	}
}

type cleanupManagerOps struct {
	manager *CobrowseSessionManager
	starts  int
}

func (o *cleanupManagerOps) Start(string) (bool, error) { o.starts++; return true, nil }
func (o *cleanupManagerOps) Observe(id string) cobrowserecovery.State {
	return cobrowserecovery.State(o.manager.RecoveryState(id))
}
func (o *cleanupManagerOps) Stop(id string) (bool, error) { return o.manager.StopRecoverySession(id) }

func TestCobrowseRecoveryJournalTerminalizesAfterCleanupRetry(t *testing.T) {
	if !cobrowserecovery.Supported() {
		t.Skip("Unix journal adapter")
	}
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	m.Recover()
	scope := cobrowserecovery.Scope{JobID: "08d55275-3b34-4514-a256-50cd3f5c92cd", Node: "1297", Org: "e91dd84f-52ef-4fdd-aa3b-c4a310cf518d", Owner: "f7176519-e74e-4b2d-b516-05d28685b03b"}
	id := "cb-08d552753b344514a25650cd3f5c92cd"
	profile := filepath.Join(m.baseDir, id)
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "Cookies"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &retryRecoveryProfile{failPersist: true}
	exited := make(chan struct{})
	stops := 0
	m.sessions[id] = &cobrowseSession{id: id, profile: profile, encProfile: p, state: SessionRunning, proc: &cobrowseProc{exited: exited, stop: func() error {
		stops++
		if stops == 1 {
			close(exited)
		}
		return nil
	}}}
	ops := &cleanupManagerOps{manager: m}
	e := cobrowserecovery.Engine{Store: cobrowserecovery.FileStore{Dir: filepath.Join(t.TempDir(), "journal")}}
	d, err := cobrowserecovery.Digest("https://example.test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Do("start", scope, d, ops); err != nil {
		t.Fatal(err)
	}
	r, err := e.Do("stop_by_job_id", scope, "", ops)
	if err == nil || r.State != cobrowserecovery.StopRequested {
		t.Fatalf("first failure became terminal: %+v %v", r, err)
	}
	// The same public recovery operation resumes cleanup from the journal.
	r, err = e.Do("job_status", scope, "", ops)
	if err != nil || r.State != cobrowserecovery.Stopped {
		t.Fatalf("retry stuck: %+v %v", r, err)
	}
	if p.persistCalls != 2 || p.closeCalls != 1 || stops != 1 || ops.starts != 1 {
		t.Fatalf("custody stages: persist=%d close=%d stop=%d start=%d", p.persistCalls, p.closeCalls, stops, ops.starts)
	}
	r, err = e.Do("start", scope, d, ops)
	if err != nil || r.State != cobrowserecovery.Stopped || ops.starts != 1 {
		t.Fatal("terminal replay relaunched")
	}
}
