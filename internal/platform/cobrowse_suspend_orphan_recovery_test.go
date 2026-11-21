package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/browserruntime"
)

func suspendRetainedFixtures() []sessionRecord {
	return []sessionRecord{
		{State: RecordSuspended},
		{State: "suspending"},
		{State: "resuming"},
		{State: RecordLive, Suspension: &suspendMetadata{}},
		{State: RecordExpired, Suspension: &suspendMetadata{}},
		{State: "future", Suspension: &suspendMetadata{}},
		{State: RecordExpired, EndedReason: "suspend_expired"},
	}
}

func TestSuspendLegacyCleanupRetentionMarkers(t *testing.T) {
	for _, rec := range suspendRetainedFixtures() {
		if !retainedSuspendRecord(rec) {
			t.Fatal("a retained marker permitted legacy cleanup", rec.State)
		}
	}
	for _, rec := range []sessionRecord{{State: RecordLive}, {State: RecordExpired}, {State: "future"}, {}} {
		if retainedSuspendRecord(rec) {
			t.Fatal("legacy marker-free classification changed", rec.State)
		}
	}
}

// Every runtime fact and engine call is fake. The real manager recovery and
// native host filesystem sweep operate only on this test's private directories.
// There are no browser launches, process kills, saved-profile handles or daemons.
func TestSuspendRecoveryProtectsRetainedResourcesFromLegacySweep(t *testing.T) {
	env := newAdoptEnv(t)
	m, reg := suspendStartFixture(t)
	f := &fakeEngine{}
	r := newTestRunner(f, false)
	f.owner = r.ownerFor(m.baseDir)
	previous := activeBrowserRunner
	activeBrowserRunner = &selectingRunner{host: newDefaultBrowserRunner(), container: r}
	t.Cleanup(func() { activeBrowserRunner = previous })

	type retained struct {
		id, dir, container string
		before             []byte
	}
	var keep []retained
	records := suspendRetainedFixtures()
	valid := suspendedTestRecord(t, false)
	if _, err := suspendExpiry(valid); err != nil {
		t.Fatal("valid suspension metadata fixture was not valid", err)
	}
	records = append(records, valid)
	for i, rec := range records {
		rec.Version = sessionRecordVersion
		rec.ID = fmt.Sprintf("cb-%016x", i+256)
		rec.Generation = 7
		dir := filepath.Join(m.baseDir, rec.ID)
		// The process owner is gone. An ordinary sweep would remove the
		// working tree, even when the runtime has already exited on Windows.
		makeSuspendSweepDirectory(t, dir)
		if err := saveRecord(reg, rec); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(recordFile(reg, rec.ID))
		if err != nil {
			t.Fatal(err)
		}
		cid := strings.Repeat(fmt.Sprintf("%x", i+1), 64)
		keep = append(keep, retained{rec.ID, dir, cid, before})
		f.psOut += cid + " " + containerName(rec.ID) + " " + rec.ID + "\n"
		f.netLsOut += browserruntime.NetworkName(rec.ID) + "\n"
	}
	// An unrelated ordinary ended session still gets normal orphan cleanup.
	deadID := "cb-ffffffffffffffff"
	deadDir := filepath.Join(m.baseDir, deadID)
	makeSuspendSweepDirectory(t, deadDir)
	if err := saveRecord(reg, sessionRecord{Version: sessionRecordVersion, ID: deadID,
		State: RecordExpired, EndedReason: "process_gone", EndedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	deadContainer := strings.Repeat("e", 64)
	f.psOut += deadContainer + " " + containerName(deadID) + " " + deadID + "\n"
	f.netLsOut += browserruntime.NetworkName(deadID) + "\n"

	m.Recover()
	for _, item := range keep {
		after, err := os.ReadFile(recordFile(reg, item.id))
		if err != nil || string(after) != string(item.before) {
			t.Fatal("legacy recovery adopted, expired or rewrote retained metadata")
		}
		if data, err := os.ReadFile(filepath.Join(item.dir, "owned-sentinel")); err != nil || string(data) != "retained working data" {
			t.Fatal("host orphan sweep removed retained working data")
		}
		if f.has("rm", "-f", item.container) != 0 || f.has("network", "rm", browserruntime.NetworkName(item.id)) != 0 {
			t.Fatal("container sweep removed a retained container/network")
		}
		if !m.sessionIsLive(item.id) {
			t.Fatal("retained metadata was classified as a removable container orphan")
		}
	}
	if len(m.sessions) != 0 {
		t.Fatal("retention marker was mistaken for authority to adopt")
	}
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatal("retention guard prevented unrelated legacy host cleanup")
	}
	if f.has("rm", "-f", deadContainer) != 1 || f.has("network", "rm", browserruntime.NetworkName(deadID)) != 1 {
		t.Fatal("retention guard prevented unrelated legacy container cleanup")
	}
	// Linux's simulated PID kills must belong only to the ordinary orphan.
	wantKills := 2
	if runtime.GOOS == "windows" {
		wantKills = 0 // The native Windows orphan sweep never kills processes.
	}
	if len(env.killed) != wantKills {
		t.Fatal("retained processes reached the legacy kill callback", len(env.killed))
	}
	if _, err := os.Stat(filepath.Join(reg, suspendManifestDirName)); !os.IsNotExist(err) {
		t.Fatal("legacy protection opened or created capture storage")
	}
}

func makeSuspendSweepDirectory(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "owned-sentinel"), []byte("retained working data"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSessionPidfile(dir, 0, 100001, 200001, 0)
	// Windows gives directories without an owner a ten-minute launch grace.
	// Expire only this synthetic directory's grace; no helper process is needed.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestSuspendRecoveryLocalSweepDoesNotReadNodeRegistry(t *testing.T) {
	env := newAdoptEnv(t)
	m, reg := suspendStartFixture(t)
	m.useResolver = false
	id := "cb-000000000000abcd"
	dir := filepath.Join(m.baseDir, id)
	makeSuspendSweepDirectory(t, dir)
	if err := saveRecord(reg, sessionRecord{Version: sessionRecordVersion, ID: id, State: "suspending"}); err != nil {
		t.Fatal(err)
	}
	registryDirResolver = func() string { t.Fatal("local sweep consulted node registry"); return reg }
	previous := activeBrowserRunner
	activeBrowserRunner = newDefaultBrowserRunner()
	t.Cleanup(func() { activeBrowserRunner = previous })
	m.Recover()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("local orphan inherited node retention")
	}
	wantKills := 2
	if runtime.GOOS == "windows" {
		wantKills = 0
	}
	if len(m.sessions) != 0 || len(env.killed) != wantKills {
		t.Fatal("local sweep changed runtime/adoption behavior")
	}
	if _, retained := sweepProtected.Load(dir); retained {
		t.Fatal("local sweep installed node recovery protection")
	}
}
