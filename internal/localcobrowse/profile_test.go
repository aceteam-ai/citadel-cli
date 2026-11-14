package localcobrowse

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

const testPassphrase = "lantern orbit pebble saffron"

var errWrongPassphrase = errors.New("that passphrase is not right")

// fakeProfile is a stand-in unlocked profile.
type fakeProfile struct{ name string }

func (p *fakeProfile) Materialize(string) error { return nil }
func (p *fakeProfile) Persist(string) error     { return nil }
func (p *fakeProfile) Close() error             { return nil }

// fakeStore is an in-memory ProfileStore. block, when set, holds Open until
// it is closed, so a test can act while an unlock is in flight.
type fakeStore struct {
	mu         sync.Mutex
	passphrase string
	opens      int
	block      chan struct{}
	entered    chan struct{}
}

func (f *fakeStore) ValidateName(name string) error {
	if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(name) {
		return errors.New("bad name")
	}
	return nil
}

func (f *fakeStore) State() (ProfileVaultState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ProfileVaultState{Configured: f.passphrase != "", Strong: f.passphrase != ""}, nil
}

func (f *fakeStore) SetPassphrase(p string, ack bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ack {
		return errors.New("acknowledgement required")
	}
	if len(p) < 16 {
		return errors.New("too short to protect data stored on disk")
	}
	f.passphrase = p
	return nil
}

func (f *fakeStore) Open(name, p string) (platform.SessionProfile, error) {
	f.mu.Lock()
	block, entered := f.block, f.entered
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.passphrase == "" || p != f.passphrase {
		return nil, errWrongPassphrase
	}
	f.opens++
	return &fakeProfile{name: name}, nil
}

func newProfileService(t *testing.T, store *fakeStore, mod func(*Config)) (*Service, *fakeBrowser, *[]Event) {
	t.Helper()
	return newTestService(t, func(c *Config) {
		c.Profiles = store
		if mod != nil {
			mod(c)
		}
	})
}

// TestProfileSessionStartsLockedAndRefusesActions: a profile session starts
// with no browser and every agent action is refused until the user unlocks.
// Mutation check: launching the browser at start puts a start call in fb.calls.
func TestProfileSessionStartsLockedAndRefusesActions(t *testing.T) {
	var reasons []string
	svc, fb, _ := newProfileService(t, &fakeStore{passphrase: testPassphrase}, func(c *Config) {
		c.OnAttention = func(_, r string) { reasons = append(reasons, r) }
	})
	res, err := svc.Start(ctx, "http://a.test/", "check orders", "default")
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateLocked || res.Profile != "default" || res.Next == "" {
		t.Fatalf("start result = %+v", res)
	}
	if len(fb.calls) != 0 {
		t.Fatalf("no browser may run while locked: %v", fb.calls)
	}
	if len(reasons) != 1 || reasons[0] != "unlock" {
		t.Fatalf("attention = %v, want [unlock]", reasons)
	}
	id := res.SessionID
	if _, err := svc.Navigate(ctx, id, "http://b.test/", "g"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("navigate: %v", err)
	}
	if _, err := svc.Click(ctx, id, "#x", nil, nil, "g"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("click: %v", err)
	}
	if _, err := svc.Type(ctx, id, "hello", "g"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("type: %v", err)
	}
	if _, err := svc.Screenshot(ctx, id, "g"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("screenshot: %v", err)
	}
	if _, err := svc.Extract(ctx, id, "body", nil, 0, "g"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("extract: %v", err)
	}
	if _, err := svc.Handoff(ctx, id, "log in", "g"); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("handoff: %v", err)
	}
	views, err := svc.Status(ctx, id)
	if err != nil || len(views) != 1 || views[0].State != StateLocked || views[0].ProfileMode != StateLocked {
		t.Fatalf("status = %+v %v", views, err)
	}
	w, err := svc.Wait(ctx, id, "unlocked", 20*time.Millisecond)
	if err != nil || w.Satisfied || !w.StillWaiting || w.State != StateLocked {
		t.Fatalf("wait while locked = %+v %v", w, err)
	}
}

// TestUnlockWithPassphraseStartsBrowserUnderSameID: the agent's id survives the
// unlock, the browser starts with the profile, and a waiting agent wakes.
func TestUnlockWithPassphraseStartsBrowserUnderSameID(t *testing.T) {
	store := &fakeStore{passphrase: testPassphrase}
	svc, fb, events := newProfileService(t, store, nil)
	res, err := svc.Start(ctx, "http://a.test/", "g", "default")
	if err != nil {
		t.Fatal(err)
	}
	id := res.SessionID
	done := make(chan WaitResult, 1)
	go func() {
		w, _ := svc.Wait(ctx, id, "unlocked", 5*time.Second)
		done <- w
	}()
	time.Sleep(20 * time.Millisecond)
	if err := svc.HumanUnlock(ctx, id, testPassphrase); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	select {
	case w := <-done:
		if !w.Satisfied || w.State != "unlocked" || w.Detail != "" {
			t.Fatalf("wait = %+v", w)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not wake on unlock")
	}
	if fb.sessions[id] == nil || fb.sessions[id].profile == nil {
		t.Fatalf("browser not started with the profile under %s: %v", id, fb.calls)
	}
	views, _ := svc.Status(ctx, id)
	if views[0].State != "running" || views[0].ProfileMode != "unlocked" {
		t.Fatalf("after unlock: %+v", views[0])
	}
	// Now the ordinary gates apply.
	if _, err := svc.Navigate(ctx, id, "http://b.test/", "g"); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("navigate after unlock: %v", err)
	}
	if err := svc.HumanUnlock(ctx, id, testPassphrase); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("second unlock: %v", err)
	}
	assertNoSecret(t, *events, testPassphrase)
}

// TestWrongPassphraseKeepsSessionLocked: a refused unlock leaves the session
// locked, is audited as refused, and never records the passphrase.
func TestWrongPassphraseKeepsSessionLocked(t *testing.T) {
	svc, fb, events := newProfileService(t, &fakeStore{passphrase: testPassphrase}, nil)
	res, _ := svc.Start(ctx, "", "g", "default")
	const wrong = "lantern orbit pebble saffrom"
	if err := svc.HumanUnlock(ctx, res.SessionID, wrong); !errors.Is(err, errWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if len(fb.calls) != 0 {
		t.Fatalf("no browser on a refused unlock: %v", fb.calls)
	}
	views, _ := svc.Status(ctx, res.SessionID)
	if views[0].State != StateLocked || views[0].Unlocking {
		t.Fatalf("after refusal: %+v", views[0])
	}
	var found bool
	for _, e := range *events {
		if e.Action == "unlock" {
			found = true
			if e.Outcome != "refused" {
				t.Fatalf("unlock outcome = %q, want refused", e.Outcome)
			}
		}
	}
	if !found {
		t.Fatal("unlock attempt not audited")
	}
	assertNoSecret(t, *events, wrong)
	// The right passphrase still works.
	if err := svc.HumanUnlock(ctx, res.SessionID, testPassphrase); err != nil {
		t.Fatalf("unlock after a wrong attempt: %v", err)
	}
}

// TestSetupPassphraseThenUnlock: with no passphrase yet, the viewer creates
// one and unlocks; setup refuses to replace an existing passphrase.
func TestSetupPassphraseThenUnlock(t *testing.T) {
	store := &fakeStore{}
	svc, fb, events := newProfileService(t, store, nil)
	res, _ := svc.Start(ctx, "", "g", "default")
	id := res.SessionID
	if err := svc.HumanSetupAndUnlock(ctx, id, "short", true); err == nil {
		t.Fatal("a weak passphrase must be refused")
	}
	if err := svc.HumanSetupAndUnlock(ctx, id, testPassphrase, false); err == nil {
		t.Fatal("setup without the data-loss acknowledgement must be refused")
	}
	if err := svc.HumanSetupAndUnlock(ctx, id, testPassphrase, true); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if fb.sessions[id] == nil {
		t.Fatal("setup did not unlock the session")
	}
	res2, _ := svc.Start(ctx, "", "g", "other")
	if err := svc.HumanSetupAndUnlock(ctx, res2.SessionID, "another long passphrase here", true); !errors.Is(err, ErrPassphraseAlreadySet) {
		t.Fatalf("setup over an existing passphrase: %v", err)
	}
	if store.passphrase != testPassphrase {
		t.Fatal("setup replaced an existing passphrase")
	}
	assertNoSecret(t, *events, testPassphrase)
}

// TestThrowawayOnlyByUserChoice: the user can run a profile session without
// the saved profile; the agent learns it is logged out.
func TestThrowawayOnlyByUserChoice(t *testing.T) {
	svc, fb, _ := newProfileService(t, &fakeStore{}, nil)
	res, _ := svc.Start(ctx, "", "g", "default")
	if err := svc.HumanUseThrowaway(ctx, res.SessionID); err != nil {
		t.Fatal(err)
	}
	if s := fb.sessions[res.SessionID]; s == nil || s.profile != nil {
		t.Fatalf("throwaway must start without a profile: %+v", s)
	}
	w, err := svc.Wait(ctx, res.SessionID, "unlocked", time.Second)
	if err != nil || !w.Satisfied || !strings.Contains(w.Detail, "logged out") {
		t.Fatalf("wait = %+v %v", w, err)
	}
	views, _ := svc.Status(ctx, res.SessionID)
	if views[0].ProfileMode != "throwaway" {
		t.Fatalf("profile mode = %q", views[0].ProfileMode)
	}
	// The saved profile is free again for another session.
	if _, err := svc.Start(ctx, "", "g", "default"); err != nil {
		t.Fatalf("profile after throwaway: %v", err)
	}
}

func TestProfileInUseAndLockedSessionsCountTowardCap(t *testing.T) {
	svc, _, _ := newProfileService(t, &fakeStore{passphrase: testPassphrase}, nil)
	if _, err := svc.Start(ctx, "", "g", "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, "", "g", "default"); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("same profile twice: %v", err)
	}
	if _, err := svc.Start(ctx, "", "g", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, "", "g", ""); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("locked sessions must count toward the cap: %v", err)
	}
	if _, err := svc.Start(ctx, "", "g", "Bad Name"); err == nil {
		t.Fatal("an invalid profile name must be refused")
	}
}

func TestStopLockedSession(t *testing.T) {
	svc, _, _ := newProfileService(t, &fakeStore{passphrase: testPassphrase}, nil)
	res, _ := svc.Start(ctx, "", "g", "default")
	if err := svc.Stop(ctx, res.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := svc.HumanUnlock(ctx, res.SessionID, testPassphrase); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unlock after stop: %v", err)
	}
}

// TestStopDuringUnlockTearsDownBrowser: a stop that lands while the unlock is
// deriving the key leaves no browser behind, and a second unlock is refused
// while the first is in flight.
func TestStopDuringUnlockTearsDownBrowser(t *testing.T) {
	store := &fakeStore{passphrase: testPassphrase, block: make(chan struct{}), entered: make(chan struct{})}
	svc, fb, _ := newProfileService(t, store, nil)
	res, _ := svc.Start(ctx, "", "g", "default")
	id := res.SessionID
	errc := make(chan error, 1)
	go func() { errc <- svc.HumanUnlock(ctx, id, testPassphrase) }()
	<-store.entered
	if err := svc.HumanUnlock(ctx, id, testPassphrase); !errors.Is(err, ErrUnlockInProgress) {
		t.Fatalf("concurrent unlock: %v", err)
	}
	if err := svc.HumanStop(id); err != nil {
		t.Fatal(err)
	}
	close(store.block)
	if err := <-errc; !errors.Is(err, ErrNoSession) {
		t.Fatalf("unlock after stop: %v", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if _, live := fb.sessions[id]; live {
		t.Fatalf("browser left running after a stop during unlock: %v", fb.calls)
	}
}

// TestUnlockLaunchFailureKeepsLocked: a browser that fails to start leaves
// the session locked, so the user can try again.
func TestUnlockLaunchFailureKeepsLocked(t *testing.T) {
	svc, fb, _ := newProfileService(t, &fakeStore{passphrase: testPassphrase}, nil)
	fb.startErr = errors.New("browser failed to start")
	res, _ := svc.Start(ctx, "", "g", "default")
	if err := svc.HumanUnlock(ctx, res.SessionID, testPassphrase); err == nil {
		t.Fatal("want launch error")
	}
	views, _ := svc.Status(ctx, res.SessionID)
	if views[0].State != StateLocked || views[0].Unlocking {
		t.Fatalf("after launch failure: %+v", views[0])
	}
}

func assertNoSecret(t *testing.T, events []Event, secret string) {
	t.Helper()
	for _, e := range events {
		if strings.Contains(fmt.Sprintf("%+v", e), secret) {
			t.Fatalf("an audit event carries the passphrase: %+v", e)
		}
	}
}

// A session that does not stop cleanly on owner exit (for example a saved
// profile that could not be sealed) is audited as an error and returned.
// Mutation check: a Close that drops Stop's error records stopped_owner_exit.
func TestCloseReportsSessionStopFailure(t *testing.T) {
	svc, fb, events := newProfileService(t, &fakeStore{passphrase: testPassphrase}, nil)
	res, _ := svc.Start(ctx, "", "g", "default")
	if err := svc.HumanUnlock(ctx, res.SessionID, testPassphrase); err != nil {
		t.Fatal(err)
	}
	fb.stopErr = errors.New("seal failed")
	if err := svc.Close(); err == nil || !strings.Contains(err.Error(), "seal failed") {
		t.Fatalf("Close = %v, want the stop failure", err)
	}
	var found bool
	for _, e := range *events {
		if e.SessionID == res.SessionID && e.Action == "stop" && e.Outcome == "error" && strings.Contains(e.Error, "seal failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failed stop was not audited: %+v", *events)
	}
}
