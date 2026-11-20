package platform

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func suspendReopenFixture(t *testing.T) (*suspendIntentStore, string, sessionRecord, sessionRecord, time.Time) {
	t.Helper()
	s, dir, expected, _ := suspendExpiryFixture(t, false)
	now := suspendTestNow.Add(time.Minute)
	planned, err := planReopenRecord(expected, expected.OwnerUserID, false, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, expected, planned, now
}

func TestSuspendReopenIntentDurableSameIDAndCapture(t *testing.T) {
	s, dir, expected, planned, now := suspendReopenFixture(t)
	var order []string
	fileSync, rename, dirSync := s.syncFile, s.rename, s.syncDir
	s.syncFile = func(f *os.File) error { order = append(order, "file"); return fileSync(f) }
	s.rename = func(a, b string) error { order = append(order, "publish"); return rename(a, b) }
	s.syncDir = func() error { order = append(order, "directory"); return dirSync() }
	intent, err := s.stageResuming(suspendPagesContext(t), expected, planned, now, func() error { return nil })
	if err != nil || !reflect.DeepEqual(order, []string{"file", "publish", "directory"}) {
		t.Fatal("reopen acknowledged before durable intent")
	}
	if intent.ID != expected.ID || intent.State != "resuming" || intent.Generation != expected.Generation+1 ||
		!reflect.DeepEqual(intent.Suspension, expected.Suspension) || intent.UpdatedAt != now.UTC().Format(time.RFC3339Nano) {
		t.Fatal("reopen changed identity, capture or retention instead of only its live generation")
	}
	manifest, err := s.payloads.read(intent, nil)
	if err != nil || manifest.Generation != expected.Generation {
		t.Fatal("resuming intent lost the immutable previous capture generation")
	}
	if !reflect.DeepEqual(planned.Suspension, expected.Suspension) || planned.UpdatedAt != expected.UpdatedAt {
		t.Fatal("persistence mutated its caller's plan")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openSuspendIntentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	current, err := reopened.read(expected.ID)
	if err != nil || !reflect.DeepEqual(current, intent) {
		t.Fatal("reopen intent did not survive an independent store reopen")
	}
	if _, err := reopened.payloads.read(current, nil); err != nil {
		t.Fatal("restart could not locate the original tab capture")
	}
}

func TestSuspendReopenIntentFailureReplay(t *testing.T) {
	for _, phase := range []string{"file-sync", "rename", "directory-sync", "cancel-after-sync"} {
		t.Run(phase, func(t *testing.T) {
			s, _, expected, planned, now := suspendReopenFixture(t)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			fileSync, rename, dirSync := s.syncFile, s.rename, s.syncDir
			refusal := errors.New("private-lifecycle-sentinel")
			switch phase {
			case "file-sync":
				s.syncFile = func(*os.File) error { return refusal }
			case "rename":
				s.rename = func(string, string) error { return refusal }
			case "directory-sync":
				s.syncDir = func() error { return refusal }
			case "cancel-after-sync":
				s.syncFile = func(f *os.File) error { cancel(); return fileSync(f) }
			}
			if out, err := s.stageResuming(ctx, expected, planned, now, func() error { return nil }); err == nil || out.ID != "" || strings.Contains(err.Error(), "private-lifecycle-sentinel") {
				t.Fatal("failed reopen commit acknowledged success or leaked provider details")
			}
			current, err := s.read(expected.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState := RecordSuspended
			if phase == "directory-sync" {
				wantState = "resuming"
			}
			if current.State != wantState {
				t.Fatal("incorrect crash-visible reopen state")
			}
			s.syncFile, s.rename, s.syncDir = fileSync, rename, dirSync
			out, err := s.stageResuming(suspendPagesContext(t), expected, planned, now.Add(time.Minute), func() error { return nil })
			if err != nil || out.Generation != expected.Generation+1 || !reflect.DeepEqual(out.Suspension, expected.Suspension) {
				t.Fatal("retry failed or refreshed capture retention")
			}
			if phase == "directory-sync" && out.UpdatedAt != current.UpdatedAt {
				t.Fatal("visible intent retry refreshed its original commit timestamp")
			}
		})
	}
}

func TestSuspendReopenIntentRequiresCurrentPlanAndPayload(t *testing.T) {
	for _, kind := range []string{"no-deadline", "cancelled", "zero-clock", "expired", "overflow", "stale-plan", "changed-capture", "wrong-state", "missing-payload", "truncated-payload", "stale-record", "nil-fence", "fence-before", "fence-after-sync"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, expected, planned, now := suspendReopenFixture(t)
			ctx, fence := suspendPagesContext(t), func() error { return nil }
			switch kind {
			case "no-deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero-clock":
				now = time.Time{}
			case "expired":
				now, _ = suspendExpiry(expected)
			case "overflow":
				expected.Generation = math.MaxUint64
			case "stale-plan":
				planned.Generation++
			case "changed-capture":
				metadata := *planned.Suspension
				metadata.TabCount++
				planned.Suspension = &metadata
			case "wrong-state":
				planned.State = RecordLive
			case "missing-payload":
				name, _ := suspendManifestName(expected)
				if err := s.payloads.root.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "truncated-payload":
				name, _ := suspendManifestName(expected)
				f, err := s.payloads.root.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			case "stale-record":
				changed := expected
				changed.Handoff = !expected.Handoff
				if err := saveRecord(dir, changed); err != nil {
					t.Fatal(err)
				}
			case "nil-fence":
				fence = nil
			case "fence-before":
				fence = func() error { return errors.New("private-lifecycle-sentinel") }
			case "fence-after-sync":
				fileSync := s.syncFile
				s.syncFile = func(f *os.File) error {
					fence = func() error { return errors.New("private-lifecycle-sentinel") }
					return fileSync(f)
				}
			}
			check := func() error {
				if fence == nil {
					return errors.New("missing lifecycle fence")
				}
				return fence()
			}
			var suppliedFence func() error = check
			if kind == "nil-fence" {
				suppliedFence = nil
			}
			if out, err := s.stageResuming(ctx, expected, planned, now, suppliedFence); err == nil || out.ID != "" || strings.Contains(err.Error(), "private-lifecycle-sentinel") {
				t.Fatal("invalid reopen plan acknowledged a transition")
			}
			current, err := s.read(expected.ID)
			if err != nil || current.State != RecordSuspended {
				t.Fatal("refused reopen replaced the suspended record")
			}
		})
	}
}

func TestSuspendReopenIntentVisibleRecordStillNeedsDurability(t *testing.T) {
	s, _, expected, planned, now := suspendReopenFixture(t)
	syncDir := s.syncDir
	s.syncDir = func() error { return errors.New("synthetic directory flush failure") }
	fence := func() error { return nil }
	if _, err := s.stageResuming(suspendPagesContext(t), expected, planned, now, fence); err == nil {
		t.Fatal("uncertain reopen intent was acknowledged")
	}
	if _, err := s.stageResuming(suspendPagesContext(t), expected, planned, now.Add(time.Second), fence); err == nil {
		t.Fatal("visible reopen intent bypassed directory durability")
	}
	s.syncDir = syncDir
	intent, err := s.stageResuming(suspendPagesContext(t), expected, planned, now.Add(time.Minute), fence)
	if err != nil || intent.UpdatedAt != now.UTC().Format(time.RFC3339Nano) {
		t.Fatal("durable reconciliation lost the original intent time")
	}
	name, _ := suspendManifestName(expected)
	if err := s.payloads.root.Remove(name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.stageResuming(suspendPagesContext(t), expected, planned, now.Add(2*time.Minute), fence); err == nil {
		t.Fatal("visible intent with missing capture was acknowledged")
	}
}

func TestSuspendReopenIntentConcurrentRetryPublishesOnce(t *testing.T) {
	s, _, expected, planned, now := suspendReopenFixture(t)
	var publications atomic.Int32
	rename := s.rename
	s.rename = func(a, b string) error { publications.Add(1); return rename(a, b) }
	var wg sync.WaitGroup
	results := make(chan sessionRecord, 2)
	errorsSeen := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.stageResuming(suspendPagesContext(t), expected, planned, now, func() error { return nil })
			results <- r
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Error(err)
		}
	}
	for r := range results {
		if r.Generation != expected.Generation+1 {
			t.Error("concurrent retry advanced generation twice")
		}
	}
	if publications.Load() != 1 {
		t.Fatal("concurrent retry republished a second reopen intent")
	}
}
