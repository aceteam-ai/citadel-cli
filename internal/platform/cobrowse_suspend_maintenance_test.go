package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func suspendMaintenanceManager(t *testing.T, dir string) *CobrowseSessionManager {
	t.Helper()
	m := newCobrowseSessionManager(trustedBaseDir(t), 2)
	m.useResolver = true
	m.sweepOnce.Do(func() {})
	previous := registryDirResolver
	registryDirResolver = func() string { return dir }
	t.Cleanup(func() { registryDirResolver = previous })
	return m
}

func TestSuspendMaintenanceScheduleBudgetAndBackoff(t *testing.T) {
	if suspendMaintenanceInterval != time.Hour || suspendMaintenanceCatchup != time.Second || suspendMaintenanceBudget != 5*time.Second {
		t.Fatal("housekeeping schedule changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, reports := 0, 0
	var waits []time.Duration
	err := runSuspendHousekeeping(ctx, func() time.Time { return suspendTestNow },
		func(_ context.Context, delay time.Duration) bool { waits = append(waits, delay); return len(waits) < 3 },
		func(passCtx context.Context, now time.Time) (bool, error) {
			calls++
			deadline, finite := passCtx.Deadline()
			if !finite || time.Until(deadline) > suspendMaintenanceBudget || now != suspendTestNow {
				t.Fatal("pass lost its budget or clock")
			}
			if calls == 2 {
				return true, errors.New("synthetic filesystem refusal")
			}
			return calls == 1, nil
		}, func() { reports++ })
	if err != nil || calls != 3 || reports != 1 || !reflect.DeepEqual(waits, []time.Duration{time.Second, time.Hour, time.Hour}) {
		t.Fatal("a failed full batch retried rapidly or a full successful batch stalled", calls, reports, waits, err)
	}
	// Cancellation during a pass must not wait or warn, and reaches its child.
	called := false
	err = runSuspendHousekeeping(ctx, func() time.Time { return suspendTestNow },
		func(context.Context, time.Duration) bool { t.Fatal("cancelled worker waited"); return true },
		func(child context.Context, _ time.Time) (bool, error) {
			called = true
			cancel()
			<-child.Done()
			return true, child.Err()
		},
		func() { t.Fatal("shutdown was reported as a filesystem failure") })
	if err != nil || !called {
		t.Fatal("worker cancellation did not end housekeeping", err)
	}
}

func TestSuspendMaintenanceMissingStoreAndLocalMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	m := suspendMaintenanceManager(t, dir)
	if more, err := m.sweepSuspendMetadata(suspendPagesContext(t), suspendTestNow); err != nil || more {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("housekeeping created an unused registry")
	}
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatal(err)
	}
	if more, err := m.sweepSuspendMetadata(suspendPagesContext(t), suspendTestNow); err != nil || more {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, suspendManifestDirName)); !os.IsNotExist(err) {
		t.Fatal("housekeeping created an unused payload store")
	}
	m.useResolver = false
	if _, err := m.sweepSuspendMetadata(suspendPagesContext(t), suspendTestNow); err == nil {
		t.Fatal("local mode entered node housekeeping")
	}
	if err := m.RunSuspendHousekeeping(context.Background()); err == nil {
		t.Fatal("local mode started a scheduler")
	}
}

func TestSuspendMaintenanceNonCreatingStoreOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if store, err := openSuspendIntentStoreWithCreate(dir, false); err == nil {
		_ = store.close()
		t.Fatal("non-creating open made a registry")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("non-creating open touched an unused registry")
	}
	if err := ensureCobrowseBaseDir(dir); err != nil {
		t.Fatal(err)
	}
	if store, err := openSuspendIntentStoreWithCreate(dir, false); err == nil {
		_ = store.close()
		t.Fatal("non-creating open made a payload store")
	}
	if _, err := os.Stat(filepath.Join(dir, suspendManifestDirName)); !os.IsNotExist(err) {
		t.Fatal("non-creating open touched an unused payload directory")
	}
}

func TestSuspendMaintenanceBatchedExpiryAndOrphans(t *testing.T) {
	s, records, now := suspendSweepFixture(t, 35)
	dir := s.root.Name()
	// A prior unreferenced capture belongs to this same private test store.
	manifest := suspendTestManifest()
	manifest.Generation++
	orphan, capture := captureRecord(t, manifest, false, nil)
	if err := s.payloads.write(orphan, capture); err != nil {
		t.Fatal(err)
	}
	orphanName, _ := suspendManifestName(orphan)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	m := suspendMaintenanceManager(t, dir)
	for i, expectedMore := range []bool{true, true, false} {
		more, err := m.sweepSuspendMetadata(suspendPagesContext(t), now)
		if err != nil || more != expectedMore {
			t.Fatal("bounded pass failed", i, more, err)
		}
	}
	for _, record := range records {
		got, err := loadRecord(dir, record.ID)
		if err != nil || got.State != RecordExpired || got.EndedReason != "suspend_expired" {
			t.Fatal("due metadata was starved", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, suspendManifestDirName, orphanName)); !os.IsNotExist(err) {
		t.Fatal("unreferenced capture survived cleanup")
	}
}

func TestSuspendMaintenancePartialFailureRetainsCursor(t *testing.T) {
	s, records, now := suspendSweepFixture(t, 35)
	dir := s.root.Name()
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	m := suspendMaintenanceManager(t, dir)
	blocked := make(map[string]bool)
	for _, record := range records[:maxSuspendTTLTransitions] {
		blocked[record.ID+".json"] = true
	}
	open := func(dir string) (*suspendIntentStore, error) {
		store, err := openSuspendIntentStoreWithCreate(dir, false)
		if err != nil {
			return nil, err
		}
		rename := store.rename
		store.rename = func(from, to string) error {
			if blocked[to] {
				return errors.New("synthetic filesystem refusal")
			}
			return rename(from, to)
		}
		return store, nil
	}
	for i := 0; i < 3; i++ {
		_, err := m.sweepSuspendMetadataWith(suspendPagesContext(t), now, open)
		if i == 0 && (err == nil || m.suspendMaintenance.ttlCursor == "") {
			t.Fatal("partial failure lost progress")
		}
	}
	for i, record := range records {
		got, err := loadRecord(dir, record.ID)
		want := RecordExpired
		if i < maxSuspendTTLTransitions {
			want = RecordSuspended
		}
		if err != nil || got.State != want {
			t.Fatal("failed entries monopolized later passes", i, err)
		}
	}
}

func TestSuspendMaintenanceRetainsTransitionalCaptures(t *testing.T) {
	s, records, now := suspendSweepFixture(t, 2)
	dir := s.root.Name()
	before := make(map[string][]byte)
	for i, record := range records {
		record.State = "suspending"
		if i == 1 {
			record.State = "resuming"
			record.Generation++
		}
		if err := saveRecord(dir, record); err != nil {
			t.Fatal(err)
		}
		before[record.ID], _ = os.ReadFile(recordFile(dir, record.ID))
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	m := suspendMaintenanceManager(t, dir)
	if more, err := m.sweepSuspendMetadata(suspendPagesContext(t), now); err != nil || more {
		t.Fatal(err)
	}
	for _, record := range records {
		after, err := os.ReadFile(recordFile(dir, record.ID))
		if err != nil || string(after) != string(before[record.ID]) {
			t.Fatal("cleanup changed an in-flight lifecycle")
		}
		name, _ := suspendManifestName(record)
		if _, err := os.Stat(filepath.Join(dir, suspendManifestDirName, name)); err != nil {
			t.Fatal("cleanup removed an in-flight capture")
		}
	}
}

func TestSuspendMaintenanceOwnershipAndRegistryWaitBudget(t *testing.T) {
	s, records, now := suspendSweepFixture(t, 1)
	dir := s.root.Name()
	m := suspendMaintenanceManager(t, dir)
	if _, err := m.sweepSuspendMetadata(suspendPagesContext(t), now); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatal("busy store was entered or leaked filesystem context")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	release, err := acquireBrowserRegistryGate(suspendPagesContext(t), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = m.sweepSuspendMetadata(ctx, now)
	release()
	if err == nil {
		t.Fatal("registry wait ignored the finite budget")
	}
	if got, err := loadRecord(dir, records[0].ID); err != nil || got.State != RecordSuspended {
		t.Fatal("refused pass changed lifecycle state")
	}
	if _, err := m.sweepSuspendMetadata(suspendPagesContext(t), now); err != nil {
		t.Fatal("refused pass leaked the store or registry lock", err)
	}
}

func TestSuspendMaintenanceSingleSchedulerAndCancellation(t *testing.T) {
	m := suspendMaintenanceManager(t, filepath.Join(t.TempDir(), "absent"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.RunSuspendHousekeeping(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		running := m.suspendMaintenance.running
		m.mu.Unlock()
		if running {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("scheduler did not start")
		}
		time.Sleep(time.Millisecond)
	}
	second, stopSecond := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopSecond()
	if err := m.RunSuspendHousekeeping(second); err == nil {
		t.Error("duplicate scheduler admitted")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler survived worker cancellation")
	}
	m.mu.Lock()
	running := m.suspendMaintenance.running
	m.mu.Unlock()
	if running {
		t.Fatal("cancelled scheduler retained its running receipt")
	}
}
