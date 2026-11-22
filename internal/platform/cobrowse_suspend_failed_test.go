package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func suspendFailureFixture(t *testing.T) (*suspendIntentStore, string, sessionRecord, preparedSuspendCapture) {
	t.Helper()
	s, dir, intent, capture := suspendCompletionFixture(t)
	name, err := suspendManifestName(intent)
	if err != nil || s.payloads.root.Remove(name) != nil {
		t.Fatal("could not prepare synthetic missing capture")
	}
	return s, dir, intent, capture
}

func TestSuspendFailureRequiresVerifiedTeardown(t *testing.T) {
	s, dir, intent, _ := suspendFailureFixture(t)
	profile := filepath.Join(filepath.Dir(dir), "untouched-profile.fixture")
	if err := os.WriteFile(profile, []byte("synthetic profile bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var order []string
	syncDir, syncFile := s.syncDir, s.syncFile
	s.syncDir = func() error { order = append(order, "registry-flush"); return syncDir() }
	s.syncFile = func(f *os.File) error { order = append(order, "record-flush"); return syncFile(f) }
	now := suspendTestNow.Add(time.Minute)
	out, err := s.failSuspending(suspendPagesContext(t), intent, now, func(ctx context.Context, identity suspendStopIdentity) error {
		if _, ok := ctx.Deadline(); !ok || identity != suspendIdentity(intent) {
			t.Error("failed suspension lost exact process facts or budget")
		}
		current, err := loadRecord(dir, intent.ID)
		if err != nil || !reflect.DeepEqual(current, intent) {
			t.Error("failure tombstone preceded verified teardown")
		}
		order = append(order, "verified-stop")
		return nil
	})
	wantOrder := []string{"registry-flush", "verified-stop", "record-flush", "registry-flush"}
	if err != nil || out.State != RecordExpired || out.EndedReason != "suspend_failed" ||
		out.EndedAt != now.Format(time.RFC3339Nano) || out.Suspension != nil || !reflect.DeepEqual(order, wantOrder) {
		t.Fatal("failed suspension did not retain its ordered terminal receipt")
	}
	if out.Generation != intent.Generation || out.OwnerUserID != intent.OwnerUserID ||
		out.ProfileName != intent.ProfileName || out.Persistent != intent.Persistent ||
		out.BrowserPID != 0 || out.XvfbPID != 0 || out.BrowserStart != 0 || out.XvfbStart != 0 ||
		out.BrowserID != "" || out.DebugPort != 0 || out.ControlPort != 0 || out.ContainerID != "" ||
		out.ContainerStart != "" || out.BrowserScope != "" || out.XvfbScope != "" || out.Runner != "" ||
		out.OwnerPID != 0 || out.OwnerStart != 0 || out.OwnerUID != nil || out.Profile != "" || out.PendingEnd != nil ||
		out.Display != "" || out.Browser != "" {
		t.Fatal("terminal receipt changed lifecycle identity or kept released resources")
	}
	if current, err := s.read(intent.ID); err != nil || !reflect.DeepEqual(current, out) {
		t.Fatal("failure receipt was acknowledged without a committed record")
	}
	if data, err := os.ReadFile(profile); err != nil || string(data) != "synthetic profile bytes" {
		t.Fatal("failure recovery changed a separate saved profile")
	}
	again, err := s.failSuspending(suspendPagesContext(t), intent, now.Add(time.Hour), func(context.Context, suspendStopIdentity) error {
		t.Error("retry of terminal receipt repeated teardown")
		return nil
	})
	if err != nil || !reflect.DeepEqual(again, out) {
		t.Fatal("idempotent failure retry refreshed the original receipt")
	}
}

func TestSuspendFailureRefusalsLeaveIntent(t *testing.T) {
	for _, kind := range []string{"deadline", "cancelled", "clock", "clock-before-capture", "callback", "state", "stale-generation", "stale-record", "identity", "present", "truncated", "directory", "payload-flush", "registry-flush", "registry-accounting"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, intent, capture := suspendFailureFixture(t)
			ctx, now, expected := suspendPagesContext(t), suspendTestNow.Add(time.Minute), intent
			calls := 0
			stop := func(context.Context, suspendStopIdentity) error { calls++; return nil }
			name, _ := suspendManifestName(intent)
			switch kind {
			case "deadline":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "clock":
				now = time.Time{}
			case "clock-before-capture":
				now = suspendTestNow.Add(-time.Second)
			case "callback":
				stop = nil
			case "state":
				expected.State = RecordLive
			case "stale-generation":
				expected.Generation++
			case "stale-record":
				expected.BrowserID = "another-incarnation"
			case "identity":
				intent.BrowserID = ""
				if err := saveRecord(dir, intent); err != nil {
					t.Fatal(err)
				}
				var err error
				intent, err = s.read(intent.ID)
				if err != nil {
					t.Fatal(err)
				}
				expected = intent
			case "present":
				if err := s.payloads.write(intent, capture); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), name), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := s.payloads.root.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
			case "payload-flush":
				s.payloads.syncDir = func() error { return errors.New("private-sentinel") }
			case "registry-flush":
				s.syncDir = func() error { return errors.New("private-sentinel") }
			case "registry-accounting":
				if err := saveRecord(dir, sessionRecord{Version: sessionRecordVersion, ID: "cb-bad", State: "unknown"}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.read(intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			out, err := s.failSuspending(ctx, expected, now, stop)
			if err == nil || out.ID != "" || calls != 0 || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("uncertain failure recovery crossed teardown or exposed details")
			}
			if current, err := s.read(intent.ID); err != nil || !reflect.DeepEqual(current, before) {
				t.Fatal("refused failure recovery rewrote the intent")
			}
		})
	}
}

func TestSuspendFailureCommitReplay(t *testing.T) {
	for _, phase := range []string{"stop", "record-sync", "rename", "directory-sync", "cancel-after-stop"} {
		t.Run(phase, func(t *testing.T) {
			s, _, intent, _ := suspendFailureFixture(t)
			ctx, cancel := context.WithCancel(suspendPagesContext(t))
			defer cancel()
			syncFile, rename, syncDir := s.syncFile, s.rename, s.syncDir
			calls := 0
			stop := func(context.Context, suspendStopIdentity) error {
				calls++
				if phase == "stop" {
					return errors.New("private-sentinel")
				}
				if phase == "cancel-after-stop" {
					cancel()
				}
				return nil
			}
			switch phase {
			case "record-sync":
				s.syncFile = func(*os.File) error { return errors.New("private-sentinel") }
			case "rename":
				s.rename = func(string, string) error { return errors.New("private-sentinel") }
			case "directory-sync":
				flushes := 0
				s.syncDir = func() error {
					flushes++
					if flushes == 2 {
						return errors.New("private-sentinel")
					}
					return syncDir()
				}
			}
			out, err := s.failSuspending(ctx, intent, suspendTestNow.Add(time.Minute), stop)
			if err == nil || out.ID != "" || calls != 1 || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("failed terminal transition was acknowledged or lost teardown")
			}
			current, err := s.read(intent.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := "suspending"
			if phase == "directory-sync" {
				want = RecordExpired
			}
			if current.State != want {
				t.Fatal("incorrect crash-visible terminal transition")
			}
			s.syncFile, s.rename, s.syncDir = syncFile, rename, syncDir
			before := calls
			out, err = s.failSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(2*time.Minute), func(context.Context, suspendStopIdentity) error { calls++; return nil })
			if err != nil || out.EndedReason != "suspend_failed" ||
				(phase == "directory-sync" && (calls != before || out.UpdatedAt != current.UpdatedAt || out.EndedAt != current.EndedAt)) {
				t.Fatal("terminal retry refreshed a receipt or repeated completed teardown")
			}
		})
	}
}

func TestSuspendFailureReappearingCaptureRemainsRecoverable(t *testing.T) {
	s, _, intent, capture := suspendFailureFixture(t)
	_, err := s.failSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(time.Minute), func(context.Context, suspendStopIdentity) error {
		return s.payloads.write(intent, capture)
	})
	if err == nil {
		t.Fatal("capture that reappeared during teardown was discarded")
	}
	if current, err := s.read(intent.ID); err != nil || !reflect.DeepEqual(current, intent) {
		t.Fatal("reappearing capture lost its original lifecycle record")
	}
	if _, err := s.completeSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(2*time.Minute), func(context.Context, suspendStopIdentity) error { return nil }); err != nil {
		t.Fatal("reappearing capture could not complete on a later pass")
	}
}

func TestSuspendFailureVisibleReceiptRefusals(t *testing.T) {
	for _, kind := range []string{"invalid-time", "mismatched-time", "another-reason", "capture-returned", "flush"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, intent, capture := suspendFailureFixture(t)
			current, err := s.failSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(time.Minute), func(context.Context, suspendStopIdentity) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "invalid-time":
				current.EndedAt, current.UpdatedAt = "not-a-time", "not-a-time"
			case "mismatched-time":
				current.UpdatedAt = suspendTestNow.Add(2 * time.Minute).Format(time.RFC3339Nano)
			case "another-reason":
				current.EndedReason = "unrelated"
			case "capture-returned":
				if err := s.payloads.write(intent, capture); err != nil {
					t.Fatal(err)
				}
			case "flush":
				s.syncDir = func() error { return errors.New("private-sentinel") }
			}
			// Inject the exact bad receipt, without the legacy writer replacing
			// UpdatedAt and accidentally testing a different timestamp guard.
			data, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(recordFile(dir, current.ID), data, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := s.failSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(3*time.Minute), func(context.Context, suspendStopIdentity) error {
				t.Error("visible terminal receipt repeated teardown")
				return nil
			})
			if err == nil || out.ID != "" || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("invalid or uncertain terminal receipt was acknowledged")
			}
			if after, err := s.read(intent.ID); err != nil || !reflect.DeepEqual(after, current) {
				t.Fatal("terminal refusal rewrote an existing receipt")
			}
		})
	}
}

func TestSuspendFailureAbsenceRecheckedBeforeStop(t *testing.T) {
	s, _, intent, capture := suspendFailureFixture(t)
	name, err := suspendManifestName(intent)
	if err != nil {
		t.Fatal(err)
	}
	flush := s.payloads.syncDir
	s.payloads.syncDir = func() error {
		if err := os.WriteFile(filepath.Join(s.payloads.root.Name(), name), capture.data, 0o600); err != nil {
			return err
		}
		return flush()
	}
	_, err = s.failSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(time.Minute), func(context.Context, suspendStopIdentity) error {
		t.Error("capture reappeared before teardown")
		return nil
	})
	if err == nil {
		t.Fatal("stale absence check admitted failure recovery")
	}
	if current, err := s.read(intent.ID); err != nil || !reflect.DeepEqual(current, intent) {
		t.Fatal("capture reappearance rewrote the pending intent")
	}
}

func TestSuspendFailureRetainsSavedProfileDescriptor(t *testing.T) {
	s, dir, intent, _ := suspendFailureFixture(t)
	meta := *intent.Suspension
	intent.Persistent, intent.ProfileName, meta.ProfileName = true, "synthetic-profile", "synthetic-profile"
	meta.ExpiresAt = suspendTestNow.Add(persistentSuspendTTL).Format(time.RFC3339Nano)
	intent.Suspension = &meta
	if err := saveRecord(dir, intent); err != nil {
		t.Fatal(err)
	}
	var err error
	intent, err = s.read(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(filepath.Dir(dir), "synthetic-kept-profile")
	if err := os.WriteFile(profile, []byte("synthetic profile bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := s.failSuspending(suspendPagesContext(t), intent, suspendTestNow.Add(time.Minute), func(context.Context, suspendStopIdentity) error { return nil })
	if err != nil || !out.Persistent || out.ProfileName != intent.ProfileName || out.EndedReason != "suspend_failed" {
		t.Fatal("failed suspension lost the retained saved-profile descriptor")
	}
	if data, err := os.ReadFile(profile); err != nil || string(data) != "synthetic profile bytes" {
		t.Fatal("terminal transition modified a separate profile artifact")
	}
}
