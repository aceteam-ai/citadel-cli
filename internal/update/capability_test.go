package update

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutoUpdaterPermissionPreflightSkipsDownloadAndLogsOnlyChanges(t *testing.T) {
	isolateState(t)
	capability := SelfUpdateCapability{State: SelfUpdatePrivileged, Directory: "/fixture/owned-by-root", BinaryOwner: "root", WorkerUser: "fixture-worker"}
	checker := &fakeChecker{release: &Release{TagName: "v9.9.9"}}
	var messages []string
	applies, restarts := 0, 0
	updater := NewAutoUpdater(AutoUpdaterConfig{
		Checker:         checker,
		Capability:      func() SelfUpdateCapability { return capability },
		HomebrewManaged: func() bool { return false },
		Apply:           func(string) error { applies++; return nil },
		Restart:         func() error { restarts++; return nil },
		Log:             func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) },
		Now:             func() time.Time { return time.Unix(2000, 0) },
	})
	for i := 0; i < 2; i++ {
		if updater.runOnce(context.Background()) {
			t.Fatal("permission refusal requested restart")
		}
	}
	if atomic.LoadInt32(&checker.downloadHits) != 0 || applies != 0 || restarts != 0 {
		t.Fatal("permission preflight happened after download/apply/restart")
	}
	state, err := LoadState()
	if err != nil || state.LastResult != ResultApplyNotPermitted || state.LastApplyCapability == nil || *state.LastApplyCapability != capability || !state.LastCheckAt.IsZero() {
		t.Fatalf("permission refusal not persisted with current identities: %+v %v", state, err)
	}
	countNotices := func() int {
		n := 0
		for _, line := range messages {
			if strings.Contains(line, "download skipped") {
				n++
			}
		}
		return n
	}
	if got := countNotices(); got != 1 {
		t.Fatalf("same refusal logged every cycle: %d", got)
	}
	capability.Directory = "/fixture/changed-install"
	updater.runOnce(context.Background())
	if got := countNotices(); got != 2 {
		t.Fatalf("changed refusal was not logged: %d", got)
	}
	blocked := capability
	capability.State = SelfUpdateAvailable
	if !updater.runOnce(context.Background()) || atomic.LoadInt32(&checker.downloadHits) != 1 || applies != 1 || restarts != 1 {
		t.Fatal("permission recovery did not permit a fake update")
	}
	capability = blocked
	updater.runOnce(context.Background())
	if got := countNotices(); got != 3 {
		t.Fatalf("refusal after recovery was incorrectly suppressed: %d", got)
	}
}

func TestAutoUpdaterUnknownCapabilitySkipsDownload(t *testing.T) {
	for _, state := range []string{SelfUpdateUnknown, "", "unexpected"} {
		t.Run(state, func(t *testing.T) {
			isolateState(t)
			checker := &fakeChecker{release: &Release{TagName: "v9.9.9"}}
			updater := NewAutoUpdater(AutoUpdaterConfig{
				Checker: checker, Capability: func() SelfUpdateCapability { return SelfUpdateCapability{State: state} },
				HomebrewManaged: func() bool { return false },
				Apply:           func(string) error { t.Fatal("unknown capability reached apply"); return nil },
				Restart:         func() error { t.Fatal("unknown capability reached restart"); return nil },
			})
			updater.runOnce(context.Background())
			if atomic.LoadInt32(&checker.downloadHits) != 0 {
				t.Fatal("unknown capability reached download")
			}
		})
	}
}
