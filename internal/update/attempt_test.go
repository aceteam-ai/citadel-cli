package update

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUpdateAttemptPrivateIsolatedAndIdempotentCleanup(t *testing.T) {
	setAttemptTestHome(t)
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
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("attempt mode = %v, %v; want 0700", info.Mode().Perm(), err)
	}
	if _, err := validatePrivateAttemptDir(a.Dir); err != nil {
		t.Fatalf("attempt platform security: %v", err)
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
	setAttemptTestHome(t)
	a, err := NewUpdateAttempt()
	if err != nil {
		t.Fatal(err)
	}
	original := a.Dir + ".original"
	if err := os.Rename(a.Dir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(a.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.Cleanup(); err == nil {
		t.Fatal("cleanup accepted replaced root")
	}
	_ = os.RemoveAll(a.Dir)
	_ = os.RemoveAll(original)
}

func setAttemptTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{"HOME", "LOCALAPPDATA", "APPDATA", "USERPROFILE"} {
		t.Setenv(name, home)
	}
}
