package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSuspendRuntimeRechecksPayloadAnchorAfterFinalProof(t *testing.T) {
	f := newSuspendRuntimeFixture(t)
	proof := f.s.proc.stopForSuspend
	retained := f.store.payloads.root.Name() + ".retained"
	var original []byte
	var payloadName string
	f.s.proc.stopForSuspend = func(ctx context.Context) error {
		if err := proof(ctx); err != nil {
			return err
		}
		if f.proofs == 2 {
			current, err := f.store.read(f.live.ID)
			if err != nil {
				t.Fatal(err)
			}
			payloadName, err = suspendManifestName(current)
			if err != nil {
				t.Fatal(err)
			}
			original, err = os.ReadFile(filepath.Join(f.store.payloads.root.Name(), payloadName))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(f.store.payloads.root.Name(), retained); err != nil {
				t.Fatal(err)
			}
			if err := EnsurePrivateDir(f.store.payloads.root.Name()); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if err := f.run(t, nil); err == nil || f.m.sessions[f.live.ID] != f.s || f.s.proc == nil {
		t.Fatal("detached payload root acknowledged or released its slot", err)
	}
	kept, err := os.ReadFile(filepath.Join(retained, payloadName))
	if err != nil || len(original) == 0 || !bytes.Equal(kept, original) {
		t.Fatal("refused late replacement destroyed the original capture")
	}
}

type suspendRuntimeFixture struct {
	m      *CobrowseSessionManager
	s      *cobrowseSession
	store  *suspendIntentStore
	live   sessionRecord
	now    time.Time
	source suspendPageSource
	exited chan struct{}
	order  []string
	proofs int
	legacy int
	close  func(context.Context, suspendBrowserClosePlan, func() error) error
	clean  func(context.Context, string) error
}

func newSuspendRuntimeFixture(t *testing.T) *suspendRuntimeFixture {
	t.Helper()
	s, _, live, source, _ := stagedCaptureFixture(t)
	base := trustedBaseDir(t)
	m := newCobrowseSessionManager(base, 4)
	m.useResolver = true
	if m.registryDir() != base+registryDirSuffix {
		t.Fatal("fixture inherited a non-private registry resolver")
	}
	store, err := openSuspendIntentStore(base + registryDirSuffix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	s.regDir = base + registryDirSuffix
	profile := filepath.Join(base, live.ID)
	if err := EnsurePrivateDir(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "synthetic-working-copy"), []byte("fake data"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.profile, s.workDir, live.Profile = profile, profile, profile
	if err := saveRecord(s.regDir, live); err != nil {
		t.Fatal(err)
	}
	live, err = store.read(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.sessions[live.ID] = s
	m.sweepOnce.Do(func() {}) // This fixture tests completion/status, not native startup recovery.
	f := &suspendRuntimeFixture{m: m, s: s, store: store, live: live, now: suspendTestNow, source: source, exited: make(chan struct{})}
	s.proc.exited = f.exited
	s.proc.stop = func() error { f.legacy++; return nil }
	s.proc.stopForSuspend = func(ctx context.Context) error {
		f.order = append(f.order, "proof")
		f.proofs++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-f.exited:
		default:
			close(f.exited)
		}
		return nil
	}
	f.close = func(ctx context.Context, _ suspendBrowserClosePlan, fence func() error) error {
		f.order = append(f.order, "graceful")
		return fence()
	}
	f.clean = func(ctx context.Context, path string) error {
		f.order = append(f.order, "clean")
		if path != profile || ctx.Err() != nil || !s.stageClosed {
			t.Fatal("cleanup lost its pinned working copy, staging barrier or budget")
		}
		if m.mu.TryLock() {
			m.mu.Unlock()
			t.Fatal("cleanup did not retain the manager slot exclusion")
		}
		if s.actionMu.TryLock() {
			s.actionMu.Unlock()
			t.Fatal("cleanup lost capture action exclusion")
		}
		short, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		if release, err := acquireBrowserRegistryGate(short, s.regDir); err == nil {
			release()
			t.Fatal("cleanup lost registry mutation exclusion")
		}
		current, err := store.read(live.ID)
		if err != nil || current.State != "suspending" || f.proofs < 1 {
			t.Fatal("cleanup preceded durable intent or positive resource proof")
		}
		return os.RemoveAll(path) // Only this fixture's private working copy.
	}
	return f
}

func (f *suspendRuntimeFixture) run(t *testing.T, alter func(sessionRecord)) error {
	t.Helper()
	return f.runContext(t, suspendPagesContext(t), alter)
}

func (f *suspendRuntimeFixture) runContext(t *testing.T, ctx context.Context, alter func(sessionRecord)) error {
	t.Helper()
	return f.s.withStagedThrowawayCapture(ctx, f.store, f.live, f.now, false, f.source,
		func() error {
			f.m.mu.Lock()
			defer f.m.mu.Unlock()
			if f.m.sessions[f.live.ID] != f.s {
				return errors.New("fixture slot changed")
			}
			return nil
		}, func(intent sessionRecord) error {
			if alter != nil {
				alter(intent)
			}
			return f.m.completeStagedThrowawaySuspend(ctx, f.s, f.store, intent,
				f.now.Add(time.Second), f.close, f.clean)
		})
}

func TestSuspendRuntimeCaptureToDurableSlotRetirement(t *testing.T) {
	f := newSuspendRuntimeFixture(t)
	f.now = time.Now().UTC() // Public status uses the real clock, unlike the pure projection.
	if err := f.run(t, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.order, []string{"graceful", "proof", "clean", "proof"}) || f.legacy != 0 ||
		len(f.m.sessions) != 0 || f.s.proc != nil || f.s.state != SessionSuspended || !f.s.watchStopped {
		t.Fatal("slot retired before complete proof, cleanup and publication", f.order)
	}
	final, err := f.store.read(f.live.ID)
	if err != nil || final.State != RecordSuspended || final.DebugPort != 0 || final.Profile != "" {
		t.Fatal("slot retired without URL-free dormant metadata")
	}
	manifest, err := f.store.payloads.read(final, nil)
	if err != nil || len(manifest.Tabs) != 2 || manifest.Generation != f.live.Generation {
		t.Fatal("completion lost its original bounded capture")
	}
	status, ok := f.m.SessionStatus(f.live.ID)
	if !ok || status.State != SessionSuspended || status.DebugPort != 0 || status.Profile != "" || status.Generation != f.live.Generation {
		t.Fatal("retired runtime is not visible through dormant manager status")
	}
	if statuses := f.m.List(); len(statuses) != 1 || statuses[0].ID != f.live.ID {
		t.Fatal("retired runtime is missing from the manager's metadata list")
	}
	assertStagedCaptureUnlocked(t, f.s, f.store)
}

func TestSuspendRuntimeFailureRetainsSlotAndCapture(t *testing.T) {
	for _, kind := range []string{"unsupported", "staging-busy", "slot-missing", "manager-busy", "wrong-profile", "manager-registry-mismatch", "proof-error", "unreaped", "cleanup-error", "cleanup-noop", "runtime-drift", "state-drift", "reaper-drift", "working-copy-replaced", "late-proof-error", "publish-uncertain"} {
		t.Run(kind, func(t *testing.T) {
			f := newSuspendRuntimeFixture(t)
			original := f.s.proc
			err := f.run(t, func(sessionRecord) {
				switch kind {
				case "unsupported":
					f.s.proc.stopForSuspend = nil
				case "staging-busy":
					f.s.stageMu.Lock()
					t.Cleanup(f.s.stageMu.Unlock)
				case "slot-missing":
					delete(f.m.sessions, f.live.ID)
				case "manager-busy":
					f.m.mu.Lock()
					t.Cleanup(f.m.mu.Unlock)
				case "wrong-profile":
					f.s.profile = t.TempDir()
				case "manager-registry-mismatch":
					previous, other := registryDirResolver, t.TempDir()
					registryDirResolver = func() string { return other }
					t.Cleanup(func() { registryDirResolver = previous })
				case "proof-error":
					f.s.proc.stopForSuspend = func(context.Context) error { return errors.New("private-runtime-sentinel") }
				case "unreaped":
					f.s.proc.stopForSuspend = func(context.Context) error { f.proofs++; return nil }
				case "cleanup-error":
					f.clean = func(context.Context, string) error { return errors.New("private-runtime-sentinel") }
				case "cleanup-noop":
					f.clean = func(context.Context, string) error { return nil }
				case "runtime-drift":
					f.close = func(context.Context, suspendBrowserClosePlan, func() error) error {
						f.s.mu.Lock()
						f.s.generation++
						f.s.mu.Unlock()
						return nil
					}
				case "state-drift":
					f.close = func(context.Context, suspendBrowserClosePlan, func() error) error {
						f.s.mu.Lock()
						f.s.state = SessionRunning
						f.s.mu.Unlock()
						return nil
					}
				case "reaper-drift":
					clean := f.clean
					f.clean = func(ctx context.Context, path string) error {
						if err := clean(ctx, path); err != nil {
							return err
						}
						other := make(chan struct{})
						close(other)
						f.s.proc.exited = other
						return nil
					}
				case "working-copy-replaced":
					f.close = func(context.Context, suspendBrowserClosePlan, func() error) error {
						if err := os.Rename(f.s.profile, f.s.profile+".retained"); err != nil {
							t.Fatal(err)
						}
						if err := EnsurePrivateDir(f.s.profile); err != nil {
							t.Fatal(err)
						}
						return nil
					}
				case "late-proof-error":
					proof := f.s.proc.stopForSuspend
					f.s.proc.stopForSuspend = func(ctx context.Context) error {
						if f.proofs == 1 {
							return errors.New("private-runtime-sentinel")
						}
						return proof(ctx)
					}
				case "publish-uncertain":
					f.store.syncDir = func() error { return errors.New("private-runtime-sentinel") }
				}
			})
			if err == nil || strings.Contains(err.Error(), "private-") || f.s.proc != original || f.legacy != 0 {
				t.Fatal("failed transaction discarded runtime, used legacy teardown, or leaked detail", err)
			}
			if (kind == "unsupported" || kind == "staging-busy" || kind == "slot-missing" || kind == "manager-busy" || kind == "wrong-profile" || kind == "manager-registry-mismatch") &&
				(len(f.order) != 0 || f.s.stageClosed) {
				t.Fatal("invalid runtime admission reached an effect")
			}
			current, readErr := f.store.read(f.live.ID)
			if readErr != nil || (current.State != "suspending" && current.State != RecordSuspended) {
				t.Fatal("failure discarded its retained lifecycle entry")
			}
			if _, readErr := f.store.payloads.read(current, nil); readErr != nil {
				t.Fatal("failure discarded its referenced capture")
			}
			if kind != "slot-missing" && kind != "manager-busy" && f.m.sessions[f.live.ID] != f.s {
				t.Fatal("failed completion released the manager slot")
			}
			if kind == "working-copy-replaced" {
				if _, err := os.Stat(f.s.profile); err != nil {
					t.Fatal("replacement working directory was removed")
				}
				if _, err := os.Stat(filepath.Join(f.s.profile+".retained", "synthetic-working-copy")); err != nil {
					t.Fatal("original working directory was removed")
				}
			}
		})
	}
}

func TestSuspendRuntimeSharesBudgetAndRetainsUncertainEffects(t *testing.T) {
	for _, phase := range []string{"graceful", "proof", "cleanup", "final-proof"} {
		t.Run(phase, func(t *testing.T) {
			f := newSuspendRuntimeFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var deadline time.Time
			observe := func(ctx context.Context, current string) {
				d, finite := ctx.Deadline()
				if !finite || time.Until(d) > suspendGracefulCloseLimit+suspendJobProofLimit {
					t.Fatal("runtime effect lost its capped shared budget")
				}
				if deadline.IsZero() {
					deadline = d
				} else if !deadline.Equal(d) {
					t.Fatal("runtime effect refreshed its deadline")
				}
				if current == phase {
					cancel()
				}
			}
			closeRoot, proof, clean := f.close, f.s.proc.stopForSuspend, f.clean
			f.close = func(ctx context.Context, plan suspendBrowserClosePlan, fence func() error) error {
				observe(ctx, "graceful")
				return closeRoot(ctx, plan, fence)
			}
			f.s.proc.stopForSuspend = func(ctx context.Context) error {
				err := proof(ctx)
				current := "proof"
				if f.proofs == 2 {
					current = "final-proof"
				}
				observe(ctx, current)
				return err
			}
			f.clean = func(ctx context.Context, path string) error {
				err := clean(ctx, path)
				observe(ctx, "cleanup")
				return err
			}
			if err := f.runContext(t, ctx, nil); err == nil || f.m.sessions[f.live.ID] != f.s || f.s.proc == nil {
				t.Fatal("cancelled transaction acknowledged or discarded runtime", err)
			}
			current, err := f.store.read(f.live.ID)
			if err != nil || current.Suspension == nil || current.Suspension.ExpiresAt != suspendTestNow.Add(throwawaySuspendTTL).Format(time.RFC3339Nano) {
				t.Fatal("cancellation lost or refreshed its retained capture")
			}
			if _, err := f.store.payloads.read(current, nil); err != nil {
				t.Fatal("cancellation discarded its referenced capture")
			}
			assertStagedCaptureUnlocked(t, f.s, f.store)
			if !f.m.mu.TryLock() {
				t.Fatal("cancelled transaction leaked manager exclusion")
			}
			f.m.mu.Unlock()
			if !f.s.stageMu.TryLock() {
				t.Fatal("cancelled transaction leaked staging exclusion")
			}
			f.s.stageMu.Unlock()
		})
	}
}

func TestSuspendRuntimeAdmissionBeforeEffects(t *testing.T) {
	for _, kind := range []string{"nil-context", "unbounded", "cancelled", "nil-manager", "nil-session", "nil-store", "nil-payload", "nil-payload-root", "nil-proc", "nil-close", "nil-clean", "zero-time", "local", "no-manifest", "persistent"} {
		t.Run(kind, func(t *testing.T) {
			f := newSuspendRuntimeFixture(t)
			err := f.s.withStagedThrowawayCapture(suspendPagesContext(t), f.store, f.live, suspendTestNow, false, f.source,
				func() error { return nil }, func(intent sessionRecord) error {
					ctx, m, s, store, now := suspendPagesContext(t), f.m, f.s, f.store, suspendTestNow.Add(time.Second)
					switch kind {
					case "nil-context":
						ctx = nil
					case "unbounded":
						ctx = context.Background()
					case "cancelled":
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					case "nil-manager":
						m = nil
					case "nil-session":
						s = nil
					case "nil-store":
						store = nil
					case "nil-payload":
						payloads := store.payloads
						store.payloads = nil
						defer func() { store.payloads = payloads }()
					case "nil-payload-root":
						root := store.payloads.root
						store.payloads.root = nil
						defer func() { store.payloads.root = root }()
					case "nil-proc":
						proc := s.proc
						s.proc = nil
						defer func() { s.proc = proc }()
					case "nil-close":
						f.close = nil
					case "nil-clean":
						f.clean = nil
					case "zero-time":
						now = time.Time{}
					case "local":
						m.useResolver = false
					case "no-manifest":
						intent.Suspension = nil
					case "persistent":
						intent.Persistent = true
					}
					return m.completeStagedThrowawaySuspend(ctx, s, store, intent, now, f.close, f.clean)
				})
			if err == nil || len(f.order) != 0 || f.s.stageClosed || f.m.sessions[f.live.ID] != f.s {
				t.Fatal("invalid completion admission affected runtime", err)
			}
		})
	}
}

func TestSuspendRuntimeStopWaitsUntilCommittedSlotRetirement(t *testing.T) {
	f := newSuspendRuntimeFixture(t)
	started, stopped := make(chan struct{}), make(chan error, 1)
	clean := f.clean
	f.clean = func(ctx context.Context, path string) error {
		go func() {
			close(started)
			stopped <- f.m.Stop(f.live.ID)
		}()
		<-started
		select {
		case <-stopped:
			t.Fatal("ordinary Stop crossed the held manager transaction")
		case <-time.After(20 * time.Millisecond):
		}
		return clean(ctx, path)
	}
	if err := f.run(t, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if err != nil || f.legacy != 0 {
			t.Fatal("late Stop ran ordinary teardown against the retired runtime", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not leave after committed slot retirement")
	}
	final, err := f.store.read(f.live.ID)
	if err != nil || final.State != RecordSuspended {
		t.Fatal("ordinary Stop erased completed lifecycle metadata")
	}
}

func TestSuspendRuntimeReconcilesUncertainPublishWithoutRefreshingCapture(t *testing.T) {
	f := newSuspendRuntimeFixture(t)
	syncDir := f.store.syncDir
	var intent sessionRecord
	if err := f.run(t, func(rec sessionRecord) {
		intent = rec
		f.store.syncDir = func() error { return errors.New("uncertain") }
	}); err == nil {
		t.Fatal("uncertain completion acknowledged")
	}
	before, err := f.store.read(f.live.ID)
	if err != nil || before.State != RecordSuspended || f.m.sessions[f.live.ID] != f.s {
		t.Fatal("uncertain publication did not retain its slot and visible record")
	}
	f.store.syncDir = syncDir
	ctx := suspendPagesContext(t)
	err = f.store.withLifecycleExclusive(ctx, func(ctx context.Context) error {
		if !f.s.actionMu.TryLock() {
			t.Fatal("failed attempt leaked action exclusion")
		}
		defer f.s.actionMu.Unlock()
		return f.m.completeStagedThrowawaySuspend(ctx, f.s, f.store, intent,
			suspendTestNow.Add(time.Hour), f.close, func(context.Context, string) error {
				t.Fatal("reconciliation repeated acknowledged cleanup")
				return nil
			})
	})
	after, readErr := f.store.read(f.live.ID)
	if err != nil || readErr != nil || !reflect.DeepEqual(before, after) || len(f.m.sessions) != 0 || f.proofs != 2 {
		t.Fatal("reconciliation refreshed TTL, skipped resource proof or kept the slot", err)
	}
}

func TestSuspendRuntimeRetryAfterCleanupFailureKeepsOriginalCapture(t *testing.T) {
	f := newSuspendRuntimeFixture(t)
	clean := f.clean
	var intent sessionRecord
	f.clean = func(context.Context, string) error { return errors.New("retryable fake cleanup failure") }
	if err := f.run(t, func(rec sessionRecord) { intent = rec }); err == nil {
		t.Fatal("failed cleanup acknowledged")
	}
	before, err := f.store.read(f.live.ID)
	if err != nil || before.State != "suspending" || f.proofs != 1 || f.m.sessions[f.live.ID] != f.s {
		t.Fatal("failed cleanup lost its live reservation or retained intent")
	}
	f.clean = clean
	ctx := suspendPagesContext(t)
	err = f.store.withLifecycleExclusive(ctx, func(ctx context.Context) error {
		if !f.s.actionMu.TryLock() {
			t.Fatal("failed cleanup leaked action exclusion")
		}
		defer f.s.actionMu.Unlock()
		return f.m.completeStagedThrowawaySuspend(ctx, f.s, f.store, intent,
			suspendTestNow.Add(time.Hour), f.close, f.clean)
	})
	final, readErr := f.store.read(f.live.ID)
	if err != nil || readErr != nil || final.State != RecordSuspended || f.proofs != 3 || len(f.m.sessions) != 0 ||
		!reflect.DeepEqual(before.Suspension, final.Suspension) {
		t.Fatal("cleanup retry discarded/refreshed capture, skipped proof or retained slot", err)
	}
	if _, err := f.store.payloads.read(final, nil); err != nil {
		t.Fatal("cleanup retry lost its original capture")
	}
}
