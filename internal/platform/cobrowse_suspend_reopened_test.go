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

func suspendReopenedFixture(t *testing.T, persistent ...bool) (*suspendIntentStore, string, sessionRecord, sessionRecord, time.Time) {
	t.Helper()
	saved := len(persistent) > 0 && persistent[0]
	s, dir, expected, _ := suspendExpiryFixture(t, saved)
	now := suspendTestNow.Add(time.Minute)
	planned, err := planReopenRecord(expected, expected.OwnerUserID, saved, now)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := s.stageResuming(suspendPagesContext(t), expected, planned, now, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	live := intent
	live.State, live.Suspension = RecordLive, nil
	live.StartedAt = now.Add(time.Second).UTC().Format(time.RFC3339Nano)
	live.Profile, live.Browser, live.BrowserID, live.DebugPort = "/synthetic/reopened", "chrome", "new-browser", 9229
	live.BrowserPID, live.BrowserStart, live.OwnerPID, live.OwnerStart = 123, 456, 789, 1234
	uid := 1000
	live.OwnerUID = &uid
	return s, dir, intent, live, now.Add(2 * time.Second)
}

func TestSuspendReopenedDurableVerifiedLive(t *testing.T) {
	for _, container := range []bool{false, true} {
		t.Run(map[bool]string{false: "host", true: "container"}[container], func(t *testing.T) {
			s, dir, intent, live, now := suspendReopenedFixture(t)
			if container {
				live.Runner, live.ContainerID, live.ContainerStart, live.ControlPort = "container", "new-container", "new-start", 9333
				live.BrowserPID, live.BrowserStart = 0, 0
			}
			before := live
			var order []string
			syncFile, rename, syncDir := s.syncFile, s.rename, s.syncDir
			s.syncFile = func(f *os.File) error { order = append(order, "file"); return syncFile(f) }
			s.rename = func(a, b string) error { order = append(order, "rename"); return rename(a, b) }
			s.syncDir = func() error { order = append(order, "directory"); return syncDir() }
			out, err := s.completeResuming(suspendPagesContext(t), intent, live, now, func() error { return nil },
				func(ctx context.Context, identity suspendStopIdentity) error {
					if _, finite := ctx.Deadline(); !finite || identity != suspendIdentity(live) {
						t.Error("restoration proof lost exact runtime identity or finite budget")
					}
					current, err := s.read(intent.ID)
					if err != nil || !reflect.DeepEqual(current, intent) {
						t.Error("restoration proof ran without its durable resuming intent")
					}
					order = append(order, "verify")
					return nil
				})
			if err != nil || !reflect.DeepEqual(order, []string{"verify", "file", "rename", "directory"}) {
				t.Fatal("live completion did not follow proof then durable publication", err)
			}
			want := live
			want.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
			if !reflect.DeepEqual(out, want) || !reflect.DeepEqual(live, before) || out.OwnerUID == live.OwnerUID {
				t.Fatal("completion changed logical identity, mutated proposal or retained its pointer")
			}
			current, err := loadRecord(dir, intent.ID)
			if err != nil || !reflect.DeepEqual(current, out) || s.payloads.checkPayload(intent) != nil {
				t.Fatal("completion was not durable or eagerly deleted capture")
			}
			// A durable live record releases its old capture for bounded cleanup.
			result, err := s.sweepOrphanPayloads(suspendPagesContext(t), "")
			if err != nil || result.Removed != 1 {
				t.Fatal("completed capture stayed charged/referenced forever", err)
			}
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := openSuspendIntentStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.close()
			current, err = restarted.read(intent.ID)
			if err != nil || !reflect.DeepEqual(current, out) {
				t.Fatal("live completion did not survive independent store reopen")
			}
		})
	}
}

func TestSuspendReopenedAfterRollbackKeepsAdvancedGeneration(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(map[bool]string{false: "throwaway", true: "persistent"}[saved], func(t *testing.T) {
			s, _, intent, live, now := suspendReopenedFixture(t, saved)
			rolledBack, err := s.rollbackResuming(suspendPagesContext(t), intent, now,
				func(context.Context, reopenAttemptIdentity) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			planned, err := planReopenRecord(rolledBack, rolledBack.OwnerUserID, saved, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			next, err := s.stageResuming(suspendPagesContext(t), rolledBack, planned, now.Add(time.Second), func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			live.Generation, live.UpdatedAt = next.Generation, next.UpdatedAt
			live.StartedAt = now.Add(2 * time.Second).Format(time.RFC3339Nano)
			out, err := s.completeResuming(suspendPagesContext(t), next, live, now.Add(3*time.Second), func() error { return nil },
				func(context.Context, suspendStopIdentity) error { return nil })
			if err != nil || out.Generation != intent.Generation+1 || out.Suspension != nil ||
				out.Persistent != saved || out.ProfileName != intent.ProfileName {
				t.Fatal("rollback/reopen completion reused generation or lost profile kind", err)
			}
		})
	}
}

func TestSuspendReopenedFailureAndVisibleReplay(t *testing.T) {
	for _, phase := range []string{"verify", "file-sync", "rename", "directory-sync", "cancel-after-verify"} {
		t.Run(phase, func(t *testing.T) {
			s, _, intent, live, now := suspendReopenedFixture(t)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			fileSync, rename, dirSync := s.syncFile, s.rename, s.syncDir
			privateErr := errors.New("private-provider-sentinel")
			proof := func(context.Context, suspendStopIdentity) error { return nil }
			switch phase {
			case "verify":
				proof = func(context.Context, suspendStopIdentity) error { return privateErr }
			case "file-sync":
				s.syncFile = func(*os.File) error { return privateErr }
			case "rename":
				s.rename = func(string, string) error { return privateErr }
			case "directory-sync":
				s.syncDir = func() error { return privateErr }
			case "cancel-after-verify":
				proof = func(context.Context, suspendStopIdentity) error { cancel(); return nil }
			}
			if out, err := s.completeResuming(ctx, intent, live, now, func() error { return nil }, proof); err == nil || out.ID != "" || strings.Contains(err.Error(), "private-provider") {
				t.Fatal("failed live completion acknowledged or leaked provider details")
			}
			current, err := s.read(intent.ID)
			want := "resuming"
			if phase == "directory-sync" {
				want = RecordLive
			}
			if err != nil || current.State != want {
				t.Fatal("wrong crash-visible live completion state")
			}
			if phase == "directory-sync" {
				// A retry cannot infer durability just from a visible rename.
				if _, err := s.completeResuming(suspendPagesContext(t), intent, live, now.Add(time.Second), func() error { return nil }, proof); err == nil {
					t.Fatal("visible live record bypassed directory flush")
				}
				name, _ := suspendManifestName(intent)
				if err := s.payloads.root.Remove(name); err != nil {
					t.Fatal(err)
				}
			}
			s.syncFile, s.rename, s.syncDir = fileSync, rename, dirSync
			proofs := 0
			out, err := s.completeResuming(suspendPagesContext(t), intent, live, now.Add(time.Minute), func() error { return nil },
				func(context.Context, suspendStopIdentity) error { proofs++; return nil })
			if err != nil || proofs != 1 || out.State != RecordLive || out.Generation != intent.Generation {
				t.Fatal("completion retry lost its incarnation or skipped fresh verification", err)
			}
			if phase == "directory-sync" && out.UpdatedAt != current.UpdatedAt {
				t.Fatal("visible commit retry refreshed completion timestamp")
			}
		})
	}
}

func TestSuspendReopenedAdmissionKeepsIntent(t *testing.T) {
	for _, kind := range []string{"nil-context", "no-deadline", "cancelled", "zero-clock", "expired", "backdated-launch", "future-launch", "backdated-intent", "state", "generation", "id", "tracking", "profile-binding", "pending-end", "ended", "metadata", "proposal-time", "browser", "port", "profile", "pid", "runner", "mixed-container", "incomplete-container", "oversized", "stale-record", "intent-resources", "intent-ended", "missing-capture", "unaccountable-registry", "nil-fence", "lost-fence", "nil-proof"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, intent, live, now := suspendReopenedFixture(t)
			original := intent
			ctx, fence := suspendPagesContext(t), func() error { return nil }
			proofs := 0
			proof := func(context.Context, suspendStopIdentity) error { proofs++; return nil }
			switch kind {
			case "nil-context":
				ctx = nil
			case "no-deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero-clock":
				now = time.Time{}
			case "expired":
				now, _ = suspendExpiry(intent)
			case "backdated-launch":
				live.StartedAt = suspendTestNow.Format(time.RFC3339Nano)
			case "future-launch":
				live.StartedAt = now.Add(time.Second).Format(time.RFC3339Nano)
			case "backdated-intent":
				intent.UpdatedAt = suspendTestNow.Add(-time.Second).Format(time.RFC3339Nano)
				live.UpdatedAt = intent.UpdatedAt
			case "state":
				intent.State = RecordSuspended
			case "generation":
				live.Generation++
			case "id":
				live.ID = "cb-abcd"
			case "tracking":
				live.Tracking = !live.Tracking
			case "profile-binding":
				live.ProfileName = "other"
			case "pending-end":
				live.PendingEnd = &EndedSession{Reason: "old"}
			case "ended":
				live.EndedReason = "old"
			case "metadata":
				live.Suspension = intent.Suspension
			case "proposal-time":
				live.UpdatedAt = now.Format(time.RFC3339Nano)
			case "browser":
				live.BrowserID = ""
			case "port":
				live.DebugPort = 65536
			case "profile":
				live.Profile = ""
			case "pid":
				live.BrowserPID = 0
			case "runner":
				live.Runner = "unknown"
			case "mixed-container":
				live.ContainerID = "other"
			case "incomplete-container":
				live.Runner, live.BrowserPID = "container", 0
			case "oversized":
				live.Profile = strings.Repeat("x", maxSuspendRegistryRecordBytes)
			case "stale-record":
				changed := intent
				changed.Tracking = !changed.Tracking
				if err := saveRecord(dir, changed); err != nil {
					t.Fatal(err)
				}
			case "intent-resources", "intent-ended":
				if kind == "intent-resources" {
					intent.BrowserID = "old-incarnation"
				} else {
					intent.EndedReason = "old-end"
				}
				if err := s.replaceRecord(original, intent, func() bool { return true }); err != nil {
					t.Fatal(err)
				}
			case "missing-capture":
				name, _ := suspendManifestName(intent)
				if err := s.payloads.root.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "unaccountable-registry":
				f, err := s.root.OpenFile("unexpected-entry", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			case "nil-fence":
				fence = nil
			case "lost-fence":
				fence = func() error { return errors.New("private-provider-sentinel") }
			case "nil-proof":
				proof = nil
			}
			if out, err := s.completeResuming(ctx, intent, live, now, fence, proof); err == nil || out.ID != "" || proofs != 0 {
				t.Fatal("invalid live proposal crossed restoration verification")
			}
			current, err := s.read(original.ID)
			if err != nil || current.State != "resuming" {
				t.Fatal("refused completion lost recoverable intent")
			}
		})
	}
}

func TestSuspendReopenedFencesAwaitAndReplay(t *testing.T) {
	for _, kind := range []string{"lost-fence", "changed-record", "lost-capture", "changed-proposal-pointer", "replay-changed-record", "replay-lost-proof", "replay-future-receipt", "replay-different-runtime", "replay-backdated-receipt"} {
		t.Run(kind, func(t *testing.T) {
			s, _, intent, live, now := suspendReopenedFixture(t)
			tamper := func(edit func(*sessionRecord)) {
				before, err := s.read(intent.ID)
				if err != nil {
					t.Fatal(err)
				}
				changed := before
				edit(&changed)
				if err := s.replaceRecord(before, changed, func() bool { return true }); err != nil {
					t.Fatal(err)
				}
			}
			fence := func() error { return nil }
			proof := func(context.Context, suspendStopIdentity) error { return nil }
			if strings.HasPrefix(kind, "replay-") {
				if _, err := s.completeResuming(suspendPagesContext(t), intent, live, now, fence, proof); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "replay-future-receipt" {
				tamper(func(current *sessionRecord) { current.UpdatedAt = now.Add(time.Hour).Format(time.RFC3339Nano) })
			}
			if kind == "replay-different-runtime" || kind == "replay-backdated-receipt" {
				tamper(func(current *sessionRecord) {
					if kind == "replay-different-runtime" {
						current.BrowserID = "different-runtime"
					} else {
						current.UpdatedAt = intent.UpdatedAt
					}
				})
			}
			proof = func(context.Context, suspendStopIdentity) error {
				switch kind {
				case "lost-fence":
					fence = func() error { return errors.New("lost") }
				case "changed-record", "replay-changed-record":
					tamper(func(current *sessionRecord) { current.Generation++ })
				case "lost-capture":
					name, _ := suspendManifestName(intent)
					if err := s.payloads.root.Remove(name); err != nil {
						t.Fatal(err)
					}
				case "changed-proposal-pointer":
					*live.OwnerUID = 2000
				case "replay-lost-proof":
					return errors.New("private-provider-sentinel")
				}
				return nil
			}
			out, err := s.completeResuming(suspendPagesContext(t), intent, live, now.Add(time.Minute), func() error { return fence() }, proof)
			if kind == "changed-proposal-pointer" {
				if err != nil || out.OwnerUID == nil || *out.OwnerUID != 1000 {
					t.Fatal("mutable proposal altered committed identity", err)
				}
			} else if err == nil || out.ID != "" {
				t.Fatal("awaited proof or replay bypassed its final fence")
			}
		})
	}
}

func TestSuspendReopenedConcurrentVerificationPublishesOnce(t *testing.T) {
	s, _, intent, live, now := suspendReopenedFixture(t)
	var proofs, publications atomic.Int32
	rename := s.rename
	s.rename = func(a, b string) error { publications.Add(1); return rename(a, b) }
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := s.completeResuming(suspendPagesContext(t), intent, live, now, func() error { return nil },
				func(context.Context, suspendStopIdentity) error { proofs.Add(1); return nil })
			if err == nil && (out.State != RecordLive || out.Generation != intent.Generation) {
				err = errors.New("concurrent completion lost identity")
			}
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if proofs.Load() != 2 || publications.Load() != 1 {
		t.Fatal("concurrent retry skipped fresh proof or published twice")
	}
}
