package platform

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func suspendCompletionFixture(t *testing.T) (*suspendIntentStore, string, sessionRecord, preparedSuspendCapture) {
	t.Helper()
	s, dir, live, capture := suspendIntentFixture(t)
	intent, err := s.stage(suspendPagesContext(t), live, live.OwnerUserID, capture, suspendTestNow, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, intent, capture
}

func TestSuspendCompletionPersistsAfterVerifiedTeardown(t *testing.T) {
	s, dir, intent, _ := suspendCompletionFixture(t)
	var order []string
	stopped := false
	syncFile, syncDir := s.syncFile, s.syncDir
	s.syncFile = func(f *os.File) error {
		if !stopped {
			t.Error("record committed before teardown")
		}
		order = append(order, "record")
		return syncFile(f)
	}
	s.syncDir = func() error { order = append(order, "directory"); return syncDir() }
	completed, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(time.Minute), func(ctx context.Context, identity suspendStopIdentity) error {
		if _, finite := ctx.Deadline(); !finite || identity != suspendIdentity(intent) {
			t.Error("teardown lost its exact process identity or budget")
		}
		r, err := loadRecord(dir, intent.ID)
		if err != nil || r.State != "suspending" || !reflect.DeepEqual(r.Suspension, intent.Suspension) {
			t.Error("teardown ran without a durable capture intent")
		}
		identity.BrowserID = "changed-local-value"
		stopped = true
		order = append(order, "stop-and-verify")
		return nil
	})
	if err != nil || completed.State != RecordSuspended || !reflect.DeepEqual(order, []string{"stop-and-verify", "record", "directory"}) {
		t.Fatal("completion did not follow the durable teardown sequence")
	}
	if completed.BrowserID != "" || completed.BrowserPID != 0 || completed.XvfbPID != 0 || completed.DebugPort != 0 ||
		completed.ControlPort != 0 || completed.BrowserStart != 0 || completed.XvfbStart != 0 || completed.BrowserScope != "" ||
		completed.XvfbScope != "" || completed.ContainerID != "" || completed.ContainerStart != "" || completed.Runner != "" ||
		completed.Profile != "" || completed.OwnerPID != 0 || completed.OwnerStart != 0 || completed.OwnerUID != nil || completed.PendingEnd != nil {
		t.Fatal("completed record retained released process resources")
	}
	if completed.Generation != intent.Generation || !reflect.DeepEqual(completed.Suspension, intent.Suspension) {
		t.Fatal("completion refreshed capture generation or TTL")
	}
	current, err := s.read(intent.ID)
	if err != nil || !reflect.DeepEqual(current, completed) {
		t.Fatal("acknowledged record was not committed")
	}
	if err := s.payloads.checkPayload(completed); err != nil {
		t.Fatal("completion lost the stored tabs")
	}
}

func TestSuspendCompletionFailureReplay(t *testing.T) {
	for _, phase := range []string{"stop", "record-sync", "rename", "directory-sync", "cancel-after-stop"} {
		t.Run(phase, func(t *testing.T) {
			s, _, intent, _ := suspendCompletionFixture(t)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			syncFile, rename, syncDir := s.syncFile, s.rename, s.syncDir
			refusal := errors.New("private-sentinel")
			calls := 0
			stop := func(context.Context, suspendStopIdentity) error { calls++; return nil }
			switch phase {
			case "stop":
				stop = func(context.Context, suspendStopIdentity) error { calls++; return refusal }
			case "record-sync":
				s.syncFile = func(*os.File) error { return refusal }
			case "rename":
				s.rename = func(string, string) error { return refusal }
			case "directory-sync":
				s.syncDir = func() error { return refusal }
			case "cancel-after-stop":
				stop = func(context.Context, suspendStopIdentity) error { calls++; cancel(); return nil }
			}
			if out, err := s.completeSuspending(ctx, intent, suspendTestNow.Add(time.Minute), stop); err == nil || out.ID != "" || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("failed completion acknowledged success or exposed provider detail")
			}
			if calls != 1 {
				t.Fatal("completion skipped teardown verification")
			}
			current, err := s.read(intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "suspending"
			if phase == "directory-sync" {
				want = RecordSuspended
			}
			if current.State != want {
				t.Fatal("incorrect crash-visible completion state")
			}
			s.syncFile, s.rename, s.syncDir = syncFile, rename, syncDir
			beforeCalls := calls
			out, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(2*time.Minute), func(context.Context, suspendStopIdentity) error { calls++; return nil })
			if err != nil || out.State != RecordSuspended {
				t.Fatal("interrupted completion could not reconcile")
			}
			if phase == "directory-sync" && (calls != beforeCalls || out.UpdatedAt != current.UpdatedAt) {
				t.Fatal("visible completed record retried teardown or refreshed its timestamp")
			}
		})
	}
}

func TestSuspendCompletionRequiresCaptureAndIntent(t *testing.T) {
	for _, kind := range []string{"no-deadline", "cancelled", "zero-clock", "no-callback", "wrong-state", "stale-generation", "stale-record", "missing-identity", "missing-payload", "truncated-payload"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, intent, _ := suspendCompletionFixture(t)
			ctx, now := suspendPagesContext(t), suspendTestNow.Add(time.Minute)
			expected := intent
			calls := 0
			stop := func(context.Context, suspendStopIdentity) error { calls++; return nil }
			switch kind {
			case "no-deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero-clock":
				now = time.Time{}
			case "no-callback":
				stop = nil
			case "wrong-state":
				expected.State = RecordLive
			case "stale-generation":
				expected.Generation++
			case "stale-record":
				expected.BrowserID = "different-browser"
			case "missing-identity":
				expected.BrowserID = ""
				if err := saveRecord(dir, expected); err != nil {
					t.Fatal(err)
				}
				var err error
				expected, err = s.read(intent.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "missing-payload":
				name, _ := suspendManifestName(intent)
				if err := s.payloads.root.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "truncated-payload":
				name, _ := suspendManifestName(intent)
				f, err := s.payloads.root.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			}
			if out, err := s.completeSuspending(ctx, expected, now, stop); err == nil || out.ID != "" || calls != 0 {
				t.Fatal("invalid completion crossed the teardown boundary")
			}
			current, err := s.read(intent.ID)
			if err != nil || current.State != "suspending" {
				t.Fatal("refused completion replaced its recoverable intent")
			}
		})
	}
}

func TestSuspendCompletionVisibleRecordStillRequiresDurability(t *testing.T) {
	s, _, intent, _ := suspendCompletionFixture(t)
	syncDir := s.syncDir
	s.syncDir = func() error { return errors.New("synthetic flush failure") }
	stopCalls := 0
	stop := func(context.Context, suspendStopIdentity) error { stopCalls++; return nil }
	if _, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow, stop); err == nil {
		t.Fatal("uncertain completion was acknowledged")
	}
	if _, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow, stop); err == nil {
		t.Fatal("visible state bypassed directory durability")
	}
	if stopCalls != 1 {
		t.Fatal("uncertain committed completion repeated teardown")
	}
	s.syncDir = syncDir
	if _, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow, stop); err != nil {
		t.Fatal("valid completed-state reconciliation failed")
	}
	name, _ := suspendManifestName(intent)
	if err := s.payloads.root.Remove(name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow, stop); err == nil {
		t.Fatal("missing capture was acknowledged on a completed-state retry")
	}
}

func TestSuspendCompletionRefusesUnaccountableRegistry(t *testing.T) {
	for _, kind := range []string{"unknown-entry", "malformed-record", "oversized-record"} {
		t.Run(kind, func(t *testing.T) {
			s, _, intent, _ := suspendCompletionFixture(t)
			name := "cb-extra.json"
			if kind == "unknown-entry" {
				name = "unrecognized-entry"
			}
			f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "oversized-record" {
				err = f.Truncate(maxSuspendRegistryRecordBytes + 1)
			} else {
				_, err = f.WriteString("not-a-lifecycle-record")
			}
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatal("could not prepare the synthetic registry entry")
			}
			calls := 0
			if _, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow,
				func(context.Context, suspendStopIdentity) error { calls++; return nil }); err == nil || calls != 0 {
				t.Fatal("unaccountable registry crossed the teardown boundary")
			}
			current, err := s.read(intent.ID)
			if err != nil || !reflect.DeepEqual(current, intent) {
				t.Fatal("refused completion changed its recoverable intent")
			}
		})
	}
}

func TestSuspendCompletionConcurrentRetryStopsOnlyOnce(t *testing.T) {
	s, _, intent, _ := suspendCompletionFixture(t)
	ctx, cancel := context.WithTimeout(suspendPagesContext(t), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	stop := func(context.Context, suspendStopIdentity) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	launch := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.completeSuspending(ctx, intent, suspendTestNow, stop)
			results <- err
		}()
	}
	launch()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("completion never reached its bounded teardown callback")
	}
	locked := !s.mu.TryLock()
	if !locked {
		s.mu.Unlock()
	}
	launch()
	unblock()
	wg.Wait()
	close(results)
	if !locked {
		t.Error("completion released lifecycle serialization across teardown")
	}
	for err := range results {
		if err != nil {
			t.Error("concurrent completion retry failed")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent retries invoked teardown more than once")
	}
}
