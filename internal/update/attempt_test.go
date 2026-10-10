package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateAttemptPrivateIsolatedAndIdempotentCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, err := NewUpdateAttempt()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewUpdateAttempt()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Cleanup()
	if a.Dir == b.Dir || filepath.Dir(a.Candidate) != a.Dir {
		t.Fatalf("attempts are not isolated: %#v %#v", a, b)
	}
	info, err := os.Stat(a.Dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("attempt mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	for _, name := range []string{filepath.Base(a.Candidate) + ".archive", "citadel-start.exe", "citadel.bat"} {
		if filepath.Dir(filepath.Join(a.Dir, name)) != a.Dir {
			t.Fatalf("artifact escaped attempt: %s", name)
		}
	}
	if err := a.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := a.Cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
}

func TestUpdateAttemptCleanupRefusesReplacedRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, err := NewUpdateAttempt()
	if err != nil {
		t.Fatal(err)
	}
	original := a.Dir + ".original"
	if err := os.Rename(a.Dir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), a.Dir); err != nil {
		t.Fatal(err)
	}
	if err := a.Cleanup(); err == nil {
		t.Fatal("cleanup accepted replaced root")
	}
	_ = os.Remove(a.Dir)
	_ = os.RemoveAll(original)
}
