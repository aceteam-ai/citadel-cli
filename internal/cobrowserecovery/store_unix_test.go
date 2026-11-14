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
		if f.Name() != "records.json" && f.Name() != "identity" {
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

func TestWholeJournalLossAndReplacementNeverConfirmStop(t *testing.T) {
	for _, kind := range []string{"removed", "renamed", "replaced", "anchor-removed", "identity-mismatch", "anchor-link", "identity-link"} {
		t.Run(kind, func(t *testing.T) {
			e, dir := testEngine(t)
			o := &fakeOps{confirmed: true}
			d := digest(t, "https://example.test", "")
			mustDo(t, e, "start", testScope, d, o)
			switch kind {
			case "removed":
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			case "renamed":
				if err := os.Rename(dir, dir+".moved"); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				if err := os.Rename(dir, dir+".moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(filepath.Join(dir, "records.json"), []byte("[]"), 0600)
				os.WriteFile(filepath.Join(dir, "identity"), []byte("affef693-24ba-49e9-bc3f-360d987b95c4"), 0600)
			case "anchor-removed":
				if err := os.Remove(dir + ".anchor"); err != nil {
					t.Fatal(err)
				}
			case "identity-mismatch":
				os.WriteFile(filepath.Join(dir, "identity"), []byte("affef693-24ba-49e9-bc3f-360d987b95c4"), 0600)
			case "anchor-link", "identity-link":
				target := dir + ".anchor"
				if kind == "identity-link" {
					target = filepath.Join(dir, "identity")
				}
				data, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				copyPath := dir + ".copy"
				if err = os.WriteFile(copyPath, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(copyPath, target); err != nil {
					t.Fatal(err)
				}
			}
			for _, action := range []string{"stop_by_job_id", "start", "job_status"} {
				r, err := e.Do(action, testScope, d, o)
				if err == nil || r.State != "" || o.starts != 1 || o.stops != 0 {
					t.Fatalf("lost continuity authorized %s: %+v %v starts=%d stops=%d", action, r, err, o.starts, o.stops)
				}
			}
		})
	}
}
