package platform

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func stagedCaptureFixture(t *testing.T) (*cobrowseSession, *suspendIntentStore, sessionRecord, suspendPageSource, *[]string) {
	t.Helper()
	store, dir, live, _ := suspendIntentFixture(t)
	s, _, source, calls := suspendPagesFixture(t, 2)
	s.id, s.ownerUserID, s.generation, s.regDir, s.profile = live.ID, live.OwnerUserID, live.Generation, dir, live.Profile
	s.proc.browserPID, s.proc.xvfbPID = live.BrowserPID, live.XvfbPID
	s.proc.browserScope, s.proc.xvfbScope, s.proc.scopeUser = live.BrowserScope, live.XvfbScope, live.ScopeUser
	s.proc.containerID, s.proc.containerStart, s.proc.containerCtrl = live.ContainerID, live.ContainerStart, live.ControlPort
	live.Tracking, live.Handoff = false, false
	if err := saveRecord(dir, live); err != nil {
		t.Fatal(err)
	}
	var err error
	live, err = store.read(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, store, live, source, calls
}

func assertStagedCaptureUnlocked(t *testing.T, s *cobrowseSession, store *suspendIntentStore) {
	t.Helper()
	if !s.actionMu.TryLock() {
		t.Fatal("staged capture leaked action exclusion")
	}
	s.actionMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := acquireBrowserRegistryGate(ctx, store.root.Name())
	if err != nil {
		t.Fatal("staged capture leaked registry exclusion")
	}
	release()
}

func TestSuspendStagedCaptureOrdersIntentBeforeContinuation(t *testing.T) {
	s, store, live, source, calls := stagedCaptureFixture(t)
	ctx := suspendPagesContext(t)
	exited := make(chan struct{})
	s.proc.exited = exited
	used, stops, fences := 0, 0, 0
	err := s.withStagedThrowawayCapture(ctx, store, live, suspendTestNow, false, source,
		func() error { fences++; return nil }, func(intent sessionRecord) error {
			used++
			current, err := store.read(live.ID)
			if err != nil || !reflect.DeepEqual(current, intent) || intent.State != "suspending" || len(*calls) == 0 {
				t.Fatal("continuation preceded complete capture and durable intent")
			}
			manifest, err := store.payloads.read(intent, nil)
			if err != nil || len(manifest.Tabs) != 2 {
				t.Fatal("continuation lost its referenced capture")
			}
			if s.actionMu.TryLock() {
				s.actionMu.Unlock()
				t.Fatal("continuation lost action exclusion")
			}
			short, cancel := context.WithTimeout(ctx, time.Millisecond)
			defer cancel()
			if release, err := acquireBrowserRegistryGate(short, store.root.Name()); err == nil {
				release()
				t.Fatal("continuation lost registry exclusion")
			}
			_, err = store.completeSuspending(ctx, intent, suspendTestNow.Add(time.Second),
				func(_ context.Context, identity suspendStopIdentity) error {
					stops++
					if identity != suspendIdentity(live) {
						t.Fatal("completion lost original runtime identity")
					}
					close(exited) // A fake proof, not an actual process operation.
					return nil
				})
			return err
		})
	if err != nil || used != 1 || stops != 1 || fences < 3 {
		t.Fatal("staged capture did not complete the trusted continuation", err)
	}
	final, err := store.read(live.ID)
	if err != nil || final.State != RecordSuspended || final.Generation != live.Generation || final.DebugPort != 0 {
		t.Fatal("completion did not retain the dormant record")
	}
	assertStagedCaptureUnlocked(t, s, store)
}

func TestSuspendStagedCaptureRejectsInputsBeforePageIO(t *testing.T) {
	for _, kind := range []string{"nil-context", "unbounded", "nil-store", "nil-fence", "nil-use", "zero-time", "persistent", "stale-record", "stale-metadata", "manager-missing", "registry-mismatch", "runtime-mismatch", "missing-reaper", "handoff-mismatch", "capture-error"} {
		t.Run(kind, func(t *testing.T) {
			s, store, live, source, calls := stagedCaptureFixture(t)
			ctx, target, now := suspendPagesContext(t), store, suspendTestNow
			fence := func() error { return nil }
			used := 0
			use := func(sessionRecord) error { used++; return nil }
			switch kind {
			case "nil-context":
				ctx = nil
			case "unbounded":
				ctx = context.Background()
			case "nil-store":
				target = nil
			case "nil-fence":
				fence = nil
			case "nil-use":
				use = nil
			case "zero-time":
				now = time.Time{}
			case "persistent":
				live.Persistent = true
			case "stale-record":
				live.Generation++
			case "stale-metadata":
				live.UpdatedAt = suspendTestNow.Add(time.Second).Format(time.RFC3339Nano)
			case "manager-missing":
				fence = func() error { return errors.New("private-sentinel") }
			case "registry-mismatch":
				s.regDir = t.TempDir()
			case "runtime-mismatch":
				s.proc.containerStart = "different-incarnation"
			case "missing-reaper":
				s.proc.exited = nil
			case "handoff-mismatch":
				s.explicitHandoff = true
			case "capture-error":
				source.verify = func(context.Context, int, string) error { return errors.New("private-sentinel") }
			}
			if err := s.withStagedThrowawayCapture(ctx, target, live, now, kind == "handoff-mismatch", source, fence, use); err == nil ||
				strings.Contains(err.Error(), "private-sentinel") || used != 0 || len(*calls) != 0 {
				t.Fatal("invalid transaction reached page I/O or continuation")
			}
			current, err := store.read(live.ID)
			if err != nil || current.State != RecordLive || current.Suspension != nil {
				t.Fatal("refused transaction published an intent")
			}
			assertStagedCaptureUnlocked(t, s, store)
		})
	}
}

func TestSuspendStagedCaptureFailureRetainsReferencedIntent(t *testing.T) {
	for _, kind := range []string{"payload-failure", "drift-after-payload", "proc-replaced-after-payload", "generation-drift-after-payload", "handoff-drift-after-payload", "flush-failure", "cancel-after-publication", "continuation-error", "continuation-cancel", "continuation-panic"} {
		t.Run(kind, func(t *testing.T) {
			s, store, live, source, _ := stagedCaptureFixture(t)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			current := true
			switch kind {
			case "payload-failure":
				store.payloads.syncDir = func() error { return errors.New("private-sentinel") }
			case "drift-after-payload":
				flush := store.payloads.syncDir
				store.payloads.syncDir = func() error { current = false; return flush() }
			case "proc-replaced-after-payload", "generation-drift-after-payload", "handoff-drift-after-payload":
				flush := store.payloads.syncDir
				store.payloads.syncDir = func() error {
					s.mu.Lock()
					if kind == "proc-replaced-after-payload" {
						replacement := *s.proc
						s.proc = &replacement
					} else if kind == "generation-drift-after-payload" {
						s.generation++
					} else {
						s.explicitHandoff = true
					}
					s.mu.Unlock()
					return flush()
				}
			case "flush-failure":
				store.syncDir = func() error { return errors.New("private-sentinel") }
			case "cancel-after-publication":
				flush := store.syncDir
				store.syncDir = func() error { err := flush(); cancel(); return err }
			}
			used, panicked := 0, false
			var err error
			func() {
				defer func() { panicked = recover() != nil }()
				err = s.withStagedThrowawayCapture(ctx, store, live, suspendTestNow, kind == "handoff-drift-after-payload", source,
					func() error {
						if !current {
							return errors.New("private-sentinel")
						}
						return nil
					}, func(sessionRecord) error {
						used++
						if kind == "continuation-panic" {
							panic("private-sentinel")
						}
						if kind == "continuation-cancel" {
							cancel()
							return nil
						}
						return errors.New("private-sentinel")
					})
			}()
			wantUse := 0
			if strings.HasPrefix(kind, "continuation-") {
				wantUse = 1
			}
			if used != wantUse || panicked != (kind == "continuation-panic") ||
				(!panicked && err == nil) || (err != nil && strings.Contains(err.Error(), "private-sentinel")) {
				t.Fatal("failed transaction crossed continuation or acknowledgement boundary", err)
			}
			rec, readErr := store.read(live.ID)
			if readErr != nil {
				t.Fatal("failed transaction lost the lifecycle record")
			}
			if kind == "payload-failure" || strings.HasSuffix(kind, "after-payload") {
				if rec.State != RecordLive || rec.Suspension != nil {
					t.Fatal("pre-intent failure replaced the live record")
				}
			} else if rec.State != "suspending" {
				t.Fatal("uncertain publication or continuation removed the intent")
			} else if manifest, err := store.payloads.read(rec, nil); err != nil || len(manifest.Tabs) != 2 {
				t.Fatal("failed continuation deleted a referenced capture")
			}
			assertStagedCaptureUnlocked(t, s, store)
		})
	}
}
