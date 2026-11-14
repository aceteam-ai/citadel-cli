//go:build !windows

package cobrowserecovery

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestJournalLockDoesNotLeakIntoBrowserExec(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	tx, err := (FileStore{Dir: dir}).Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	f := tx.(*fileTx).lock
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("lock could survive browser exec: %x %v", flags, err)
	}
}
func TestCrashStagingIsReusedAndBounded(t *testing.T) {
	e, dir := testEngine(t)
	tx, err := e.Store.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Close()
	old := filepath.Join(dir, "records.next")
	if err = os.WriteFile(old, make([]byte, 65536), 0600); err != nil {
		t.Fatal(err)
	}
	mustDo(t, e, "stop_by_job_id", testScope, "", &fakeOps{})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("stale staging survives: %v", entries)
	}
	for _, f := range entries {
		if f.Name() != "records.json" && f.Name() != "journal.lock" {
			t.Fatal("unexpected staged file")
		}
	}
}

func TestMissingJournalNeverBecomesFreshNotFound(t *testing.T) {
	e, dir := testEngine(t)
	mustDo(t, e, "start", testScope, digest(t, "https://example.test", ""), &fakeOps{})
	if err := os.Remove(filepath.Join(dir, "records.json")); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "job_status", "stop_by_job_id"} {
		o := &fakeOps{confirmed: true}
		r, err := e.Do(action, testScope, digest(t, "https://example.test", ""), o)
		if err == nil || r.State != State("") || o.starts != 0 || o.stops != 0 {
			t.Fatalf("missing continuity authorized %s: %+v %v", action, r, err)
		}
	}
}
