package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func browserRegistryGateRefs(t *testing.T, dir string) int {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	browserRegistryGates.Lock()
	defer browserRegistryGates.Unlock()
	for g := range browserRegistryGates.entries {
		if os.SameFile(info, g.info) {
			return g.refs
		}
	}
	return 0
}

// Wait for a registered waiter, not a guessed scheduling delay. An unfenced
// writer finishes instead and fails the proof immediately.
func waitBrowserRegistryWriter(t *testing.T, dir string, done <-chan error) (waiting, consumed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if browserRegistryGateRefs(t, dir) >= 2 {
			return true, false
		}
		select {
		case <-done:
			t.Error("legacy writer bypassed the active registry transaction")
			return false, true
		case <-ctx.Done():
			t.Error("legacy writer did not register its bounded gate wait")
			return false, false
		case <-tick.C:
		}
	}
}

func TestSuspendRegistryGateLegacyExclusion(t *testing.T) {
	for _, kind := range []string{"save", "delete", "persist"} {
		t.Run(kind, func(t *testing.T) {
			s, original := suspendWriterFixture(t)
			store, err := openSuspendIntentStore(s.regDir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			done := make(chan error, 1)
			writes := 0
			completed := false
			err = store.withLifecycleExclusive(suspendPagesContext(t), func(context.Context) error {
				go func() {
					switch kind {
					case "save":
						next := original
						next.Generation++
						done <- saveRecord(s.regDir, next)
					case "delete":
						deleteRecord(s.regDir, s.id)
						done <- nil
					case "persist":
						s.persistRecordWithWriter(func(string, sessionRecord) error { writes++; return nil })
						done <- nil
					}
				}()
				waiting, consumed := waitBrowserRegistryWriter(t, s.regDir, done)
				completed = consumed
				if !waiting {
					return errors.New("unfenced writer")
				}
				// Gate BEFORE session mutex: a waiting live writer must not hold
				// the mutex the transaction needs, or the order would deadlock.
				if !s.mu.TryLock() {
					t.Error("waiting live writer inverted registry/session lock order")
					return errors.New("inverted lock order")
				}
				if kind == "persist" {
					s.generation++
				}
				s.mu.Unlock()
				got, err := loadRecord(s.regDir, s.id)
				if err != nil || !reflect.DeepEqual(got, original) {
					t.Error("legacy writer changed the record inside a transaction")
				}
				return nil
			})
			if err != nil {
				t.Error("registry transaction did not complete")
			}
			if !completed {
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("writer did not resume after transaction release")
				}
			}
			if browserRegistryGateRefs(t, s.regDir) != 0 {
				t.Fatal("completed registry writer leaked its gate")
			}
			if !completed && kind == "persist" && writes != 0 {
				t.Fatal("live writer failed to recheck generation after its gate wait")
			}
		})
	}
}

func TestSuspendRegistryGateBudgetAndRelease(t *testing.T) {
	store, _, _ := suspendSweepFixture(t, 0)
	ctx := suspendPagesContext(t)
	for _, dir := range []string{filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "file")} {
		if filepath.Base(dir) == "file" {
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if r, err := acquireBrowserRegistryGate(ctx, dir); err == nil || r != nil || strings.Contains(err.Error(), dir) {
			if r != nil {
				r()
			}
			t.Fatal("invalid registry path acquired a gate or leaked the path")
		}
	}
	for _, kind := range []string{"nil-context", "no-deadline", "cancelled", "nil-callback", "callback-error", "cancel-after-callback"} {
		t.Run(kind, func(t *testing.T) {
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			calls := 0
			run := func(context.Context) error { calls++; return nil }
			switch kind {
			case "nil-context":
				callCtx = nil
			case "no-deadline":
				callCtx = context.Background()
			case "cancelled":
				cancel()
			case "nil-callback":
				run = nil
			case "callback-error":
				run = func(context.Context) error { calls++; return errors.New("private-gate-sentinel") }
			case "cancel-after-callback":
				run = func(context.Context) error { calls++; cancel(); return nil }
			}
			err := store.withLifecycleExclusive(callCtx, run)
			wantCalls := 0
			if kind == "callback-error" || kind == "cancel-after-callback" {
				wantCalls = 1
			}
			if err == nil || strings.Contains(err.Error(), "private-gate-sentinel") || calls != wantCalls || browserRegistryGateRefs(t, store.root.Name()) != 0 {
				t.Fatal("invalid/failed transaction acknowledged, leaked details, ran effects or retained its gate")
			}
		})
	}
	// A finite budget includes time WAITING for the exclusive registry gate.
	release, err := acquireBrowserRegistryGate(ctx, store.root.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	calls := 0
	if err := store.withLifecycleExclusive(short, func(context.Context) error { calls++; return nil }); err == nil || calls != 0 {
		t.Fatal("expired waiter performed or acknowledged a transaction")
	}
	if browserRegistryGateRefs(t, store.root.Name()) != 1 {
		t.Fatal("cancelled waiter leaked a registry gate reference")
	}
	release()
	release() // release is idempotent, not another token or a negative refcount
	if err := store.withLifecycleExclusive(ctx, func(context.Context) error { return nil }); err != nil || browserRegistryGateRefs(t, store.root.Name()) != 0 {
		t.Fatal("released gate could not be reused and reclaimed")
	}
}

func TestSuspendRegistryGateIdentityAndBound(t *testing.T) {
	if maxBrowserRegistryGates != 64 {
		t.Fatal("registry gate memory bound changed without a contract update")
	}
	ctx := suspendPagesContext(t)
	dir := t.TempDir()
	release, err := acquireBrowserRegistryGate(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err == nil {
		short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		other, err := acquireBrowserRegistryGate(short, alias)
		cancel()
		if other != nil {
			other()
		}
		if err == nil || other != nil || browserRegistryGateRefs(t, dir) != 1 {
			t.Fatal("a directory alias bypassed registry exclusion")
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	// Distinct directories do not share a gate; active entries stay bounded.
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 1; i < maxBrowserRegistryGates; i++ {
		r, err := acquireBrowserRegistryGate(ctx, t.TempDir())
		if err != nil {
			t.Fatal("independent registry was serialized or refused below the bound")
		}
		releases = append(releases, r)
	}
	extra := t.TempDir()
	if r, err := acquireBrowserRegistryGate(ctx, extra); err == nil || r != nil {
		if r != nil {
			r()
		}
		t.Fatal("active registry gates exceeded the bounded table")
	}
	release()
	r, err := acquireBrowserRegistryGate(ctx, extra)
	if err != nil {
		t.Fatal("reclaimed entry did not restore registry gate capacity")
	}
	r()
}

func TestSuspendRegistryGateRejectsReplacedDirectory(t *testing.T) {
	store, _, _ := suspendSweepFixture(t, 0)
	dir := store.root.Name()
	ctx := suspendPagesContext(t)
	release, err := acquireBrowserRegistryGate(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() {
		r, err := acquireBrowserRegistryGate(ctx, dir)
		if r != nil {
			r()
		}
		done <- err
	}()
	waiting, consumed := waitBrowserRegistryWriter(t, dir, done)
	if !waiting {
		release()
		if !consumed {
			<-done
		}
		t.Fatal("replacement fixture never queued its gate waiter")
	}
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		release()
		<-done
		if runtime.GOOS == "windows" {
			t.Skip("platform does not allow renaming the open private test directory")
		}
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		release()
		<-done
		t.Fatal(err)
	}
	release()
	if err := <-done; err == nil {
		t.Fatal("waiting operation used a different directory under the same name")
	}
	calls := 0
	if err := store.withLifecycleExclusive(ctx, func(context.Context) error { calls++; return nil }); err == nil || calls != 0 {
		t.Fatal("transaction used a detached anchored registry under another gate")
	}
}

func TestSuspendRegistryGateAnchoredCompletion(t *testing.T) {
	store, _, intent, _ := suspendCompletionFixture(t)
	var completed sessionRecord
	err := store.withLifecycleExclusive(suspendPagesContext(t), func(ctx context.Context) error {
		var err error
		completed, err = store.completeSuspending(ctx, intent, suspendTestNow.Add(time.Minute), func(context.Context, suspendStopIdentity) error { return nil })
		return err
	})
	if err != nil || completed.State != RecordSuspended {
		t.Fatal("anchored lifecycle transition deadlocked or failed under registry exclusion")
	}
	got, err := store.read(intent.ID)
	if err != nil || !reflect.DeepEqual(got, completed) || browserRegistryGateRefs(t, store.root.Name()) != 0 {
		t.Fatal("gated anchored transition was not committed or retained its gate")
	}
	for _, kind := range []string{"registry-replaced", "payload-replaced"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := suspendSweepFixture(t, 0)
			path := s.root.Name()
			if kind == "payload-replaced" {
				path = filepath.Join(path, suspendManifestDirName)
			}
			calls, moved := 0, false
			err := s.withLifecycleExclusive(suspendPagesContext(t), func(context.Context) error {
				calls++
				if err := os.Rename(path, path+"-moved"); err != nil {
					return err
				}
				moved = true
				return os.Mkdir(path, 0o700)
			})
			if !moved && runtime.GOOS == "windows" {
				t.Skip("platform does not allow moving the open private test directory")
			}
			if err == nil || calls != 1 {
				t.Fatal("transaction acknowledged a detached registry/payload after its callback")
			}
		})
	}
}
