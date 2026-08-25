package platform

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeProfile is a test SessionProfile. It records call ordering and counts so a
// test can assert Materialize-before-launch, Persist-only-if-launched, and
// always-Close. Materialize drops a sentinel file so the fake launcher can confirm
// the profile was decrypted into the working dir BEFORE the browser started.
type fakeProfile struct {
	mu             sync.Mutex
	materialized   int
	persisted      int
	closed         int
	materializeErr error
}

const materializeSentinel = ".materialized-sentinel"

func (p *fakeProfile) Materialize(dir string) error {
	p.mu.Lock()
	p.materialized++
	p.mu.Unlock()
	if p.materializeErr != nil {
		return p.materializeErr
	}
	return os.WriteFile(filepath.Join(dir, materializeSentinel), []byte("x"), 0o600)
}

func (p *fakeProfile) Persist(dir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persisted++
	return nil
}

func (p *fakeProfile) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
	return nil
}

func (p *fakeProfile) counts() (m, per, c int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.materialized, p.persisted, p.closed
}

// sentinelCheckingLauncher records, per launch, whether the working dir already
// contained the materialize sentinel — proving Materialize ran before launch.
type sentinelCheckingLauncher struct {
	mu          sync.Mutex
	sawSentinel bool
	launchErr   error
	stopped     int
}

func (l *sentinelCheckingLauncher) install(t *testing.T) {
	t.Helper()
	prev := launchCobrowseProc
	launchCobrowseProc = func(profileDir, startURL string) (*cobrowseProc, error) {
		if l.launchErr != nil {
			return nil, l.launchErr
		}
		_, err := os.Stat(filepath.Join(profileDir, materializeSentinel))
		l.mu.Lock()
		l.sawSentinel = err == nil
		l.mu.Unlock()
		exited := make(chan struct{})
		return &cobrowseProc{
			debugPort:  9100,
			display:    ":91",
			browserPID: 123,
			xvfbPID:    456,
			exited:     exited,
			stop: func() error {
				l.mu.Lock()
				l.stopped++
				l.mu.Unlock()
				return nil
			},
		}, nil
	}
	t.Cleanup(func() { launchCobrowseProc = prev })
}

// TestPersistentProfile_MaterializeThenPersist is the core wiring acceptance test:
// the profile is decrypted into the working dir BEFORE the browser launches, and on
// Stop it is re-encrypted (Persist) then Closed, and the plaintext working dir is
// removed. Mutation checks: materialize-after-launch flips sawSentinel; skipping
// Persist-on-stop drops the persisted count; a leaked Close drops closed.
func TestPersistentProfile_MaterializeThenPersist(t *testing.T) {
	l := &sentinelCheckingLauncher{}
	l.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	p := &fakeProfile{}

	st, err := m.StartSessionWithProfile("https://x.example", p)
	if err != nil {
		t.Fatalf("StartSessionWithProfile: %v", err)
	}

	l.mu.Lock()
	saw := l.sawSentinel
	l.mu.Unlock()
	if !saw {
		t.Error("profile was not materialized into the working dir before launch")
	}
	if mCount, per, _ := p.counts(); mCount != 1 || per != 0 {
		t.Errorf("after start: materialized=%d persisted=%d, want 1,0", mCount, per)
	}

	if err := m.Stop(st.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	mCount, per, c := p.counts()
	if mCount != 1 || per != 1 || c != 1 {
		t.Errorf("after stop: materialized=%d persisted=%d closed=%d, want 1,1,1", mCount, per, c)
	}
	// Plaintext working copy must be gone.
	if _, err := os.Stat(st.Profile); !os.IsNotExist(err) {
		t.Errorf("plaintext working dir left behind after stop (err=%v)", err)
	}
}

// TestPersistentProfile_FailedLaunchNoPersistButClose verifies a launch that never
// produced a browser does NOT persist (an empty/partial dir must not overwrite good
// ciphertext) but DOES Close the profile (no leaked key/lock) and remove the dir.
// Mutation check: persisting on the failed-launch path bumps persisted to 1.
func TestPersistentProfile_FailedLaunchNoPersistButClose(t *testing.T) {
	l := &sentinelCheckingLauncher{launchErr: errors.New("boom")}
	l.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	p := &fakeProfile{}

	st, err := m.StartSessionWithProfile("https://x.example", p)
	if err == nil {
		t.Fatal("expected StartSessionWithProfile to fail")
	}
	_, per, c := p.counts()
	if per != 0 {
		t.Errorf("persisted=%d on failed launch, want 0 (empty dir must not overwrite store)", per)
	}
	if c != 1 {
		t.Errorf("closed=%d on failed launch, want 1 (key/lock must not leak)", c)
	}
	if st.Profile != "" {
		if _, err := os.Stat(st.Profile); !os.IsNotExist(err) {
			t.Errorf("working dir left behind after failed launch")
		}
	}
}

// TestPersistentProfile_MaterializeErrorFailsClosed verifies that if decrypting the
// profile fails (e.g. wrong context / corrupt store surfaced by Materialize), the
// session does not start and the profile is Closed. Fail-closed: no browser, no
// plaintext.
func TestPersistentProfile_MaterializeErrorFailsClosed(t *testing.T) {
	l := &sentinelCheckingLauncher{}
	l.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 8)
	p := &fakeProfile{materializeErr: errors.New("unseal failed")}

	if _, err := m.StartSessionWithProfile("https://x.example", p); err == nil {
		t.Fatal("expected start to fail when Materialize fails")
	}
	if _, per, c := p.counts(); per != 0 || c != 1 {
		t.Errorf("on materialize failure: persisted=%d closed=%d, want 0,1", per, c)
	}
	if got := len(m.List()); got != 0 {
		t.Errorf("a session was registered despite materialize failure: %d", got)
	}
}

// TestPersistentProfile_CapExceededClosesProfile verifies that when the session cap
// is hit, the profile handed in is Closed (not leaked) since the manager refuses to
// take it.
func TestPersistentProfile_CapExceededClosesProfile(t *testing.T) {
	l := &sentinelCheckingLauncher{}
	l.install(t)
	m := newCobrowseSessionManager(trustedBaseDir(t), 1)

	// Fill the single slot with a throwaway session.
	if _, err := m.StartSession("https://a.example"); err != nil {
		t.Fatalf("first start: %v", err)
	}
	p := &fakeProfile{}
	if _, err := m.StartSessionWithProfile("https://b.example", p); err == nil {
		t.Fatal("expected cap-exceeded error")
	}
	if _, _, c := p.counts(); c != 1 {
		t.Errorf("profile not Closed on cap-exceeded: closed=%d, want 1", c)
	}
}
