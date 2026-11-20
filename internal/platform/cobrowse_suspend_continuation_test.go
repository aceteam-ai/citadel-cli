package platform

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSuspendCaptureContinuationRetainsActionExclusion(t *testing.T) {
	s, targets, source, _ := suspendPagesFixture(t, 2)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	ctx := suspendPagesContext(t)
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	go func() {
		done <- s.withSuspendPageCapture(ctx, s.ownerUserID, false, source, func(m suspendManifest) error {
			if m.SessionID != s.id || m.Generation != s.generation || len(m.Tabs) != len(targets) {
				return errors.New("capture lost its complete plan")
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("continuation did not run", err)
	case <-ctx.Done():
		t.Fatal("continuation did not start within its budget")
	}
	if s.actionMu.TryLock() {
		s.actionMu.Unlock()
		t.Fatal("capture released action exclusion before continuation")
	}
	if m, err := s.captureSuspendPagesWith(ctx, s.ownerUserID, false, source); err == nil || len(m.Tabs) != 0 {
		t.Fatal("competing capture entered the retained action")
	}
	finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("complete continuation was not acknowledged", err)
		}
	case <-ctx.Done():
		t.Fatal("continuation did not finish within its budget")
	}
	if !s.actionMu.TryLock() {
		t.Fatal("complete continuation leaked action exclusion")
	}
	s.actionMu.Unlock()
}

func TestSuspendCaptureContinuationRejectsBeforeIO(t *testing.T) {
	for _, kind := range []string{"nil-context", "unbounded", "nil-use", "nil-provider", "capture-failure"} {
		t.Run(kind, func(t *testing.T) {
			s, _, source, calls := suspendPagesFixture(t, 1)
			ctx := suspendPagesContext(t)
			used := false
			use := func(suspendManifest) error { used = true; return nil }
			switch kind {
			case "nil-context":
				ctx = nil
			case "unbounded":
				ctx = context.Background()
			case "nil-use":
				use = nil
			case "nil-provider":
				source.read = nil
			case "capture-failure":
				source.verify = func(context.Context, int, string) error { return errors.New("private-sentinel") }
			}
			if err := s.withSuspendPageCapture(ctx, s.ownerUserID, false, source, use); err == nil ||
				strings.Contains(err.Error(), "private-sentinel") || used || len(*calls) != 0 {
				t.Fatal("refused capture reached continuation or leaked provider data")
			}
			if !s.actionMu.TryLock() {
				t.Fatal("refused capture leaked action exclusion")
			}
			s.actionMu.Unlock()
		})
	}
}

func TestSuspendCaptureContinuationFailureAndCancellation(t *testing.T) {
	for _, kind := range []string{"success", "error", "cancel", "panic"} {
		t.Run(kind, func(t *testing.T) {
			s, _, source, _ := suspendPagesFixture(t, 1)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			used := 0
			panicked := false
			var err error
			func() {
				defer func() { panicked = recover() != nil }()
				err = s.withSuspendPageCapture(ctx, s.ownerUserID, false, source, func(suspendManifest) error {
					used++
					switch kind {
					case "error":
						return errors.New("private-sentinel")
					case "cancel":
						cancel()
					case "panic":
						panic("private-sentinel")
					}
					return nil
				})
			}()
			if used != 1 || panicked != (kind == "panic") ||
				(kind == "success" && err != nil) ||
				((kind == "error" || kind == "cancel") && err == nil) ||
				(err != nil && strings.Contains(err.Error(), "private-sentinel")) {
				t.Fatal("continuation acknowledgement or error contract changed")
			}
			if !s.actionMu.TryLock() {
				t.Fatal("continuation exit leaked action exclusion")
			}
			s.actionMu.Unlock()
		})
	}
}

// Component integration, not a manager transaction or browser-stop proof. The
// fake page source is composed with real private payload/intent persistence.
func TestSuspendCaptureContinuationStagesWithoutUnlockGap(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable", true: "cancel-after-publication"}[interrupted], func(t *testing.T) {
			store, dir, live, _ := suspendIntentFixture(t)
			s, _, source, _ := suspendPagesFixture(t, 2)
			s.id, s.ownerUserID, s.generation, s.regDir = live.ID, live.OwnerUserID, live.Generation, dir
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			flushes := 0
			flush := store.syncDir
			store.syncDir = func() error {
				flushes++
				if s.actionMu.TryLock() {
					s.actionMu.Unlock()
					return errors.New("intent publication lost action exclusion")
				}
				wait, stop := context.WithTimeout(ctx, time.Millisecond)
				defer stop()
				if release, err := acquireBrowserRegistryGate(wait, dir); err == nil {
					release()
					return errors.New("intent publication lost registry exclusion")
				}
				if err := flush(); err != nil {
					return err
				}
				if interrupted {
					cancel()
				}
				return nil
			}
			err := store.withLifecycleExclusive(ctx, func(ctx context.Context) error {
				return s.withSuspendPageCapture(ctx, s.ownerUserID, false, source, func(m suspendManifest) error {
					capture, err := prepareSuspendCapture(m, false, "", nil)
					if err != nil {
						return err
					}
					defer capture.clear()
					_, err = store.stage(ctx, live, live.OwnerUserID, capture, suspendTestNow, func() error { return ctx.Err() })
					return err
				})
			})
			if (err != nil) != interrupted || flushes != 1 {
				t.Fatal("intent did not preserve exclusion through durable publication", err)
			}
			rec, err := store.read(live.ID)
			if err != nil || rec.State != "suspending" || rec.Generation != live.Generation {
				t.Fatal("publication lost the recoverable intent")
			}
			manifest, err := store.payloads.read(rec, nil)
			if err != nil || len(manifest.Tabs) != 2 {
				t.Fatal("publication lost its complete referenced payload")
			}
			if !s.actionMu.TryLock() {
				t.Fatal("intent continuation leaked action exclusion")
			}
			s.actionMu.Unlock()
		})
	}
}
