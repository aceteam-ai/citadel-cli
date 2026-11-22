package service

import (
	"errors"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/worklock"
)

func TestBuildRunValueQuotesPathWithSpaces(t *testing.T) {
	got, err := BuildRunValue(`C:\Users\Test User\AppData\Local\Citadel\citadel.exe`)
	if err != nil {
		t.Fatal(err)
	}
	want := `"C:\Users\Test User\AppData\Local\Citadel\citadel.exe" work --background`
	if got != want {
		t.Fatalf("BuildRunValue = %q, want %q", got, want)
	}
}

func TestBuildRunValueRefusesUnsafePaths(t *testing.T) {
	for _, p := range []string{
		"",
		"   ",
		"citadel.exe",
		`bin\citadel.exe`,
		`.\citadel.exe`,
		`C:citadel.exe`,
		`C:\Program Files\"evil\citadel.exe`,
		"C:\\citadel.exe\r\nwork",
	} {
		if v, err := BuildRunValue(p); err == nil {
			t.Errorf("BuildRunValue(%q) = %q, want an error", p, v)
		}
	}
}

func TestBuildRunValueAcceptsAbsoluteForms(t *testing.T) {
	for _, p := range []string{`C:\citadel.exe`, `d:/tools/citadel.exe`, `\\server\share\citadel.exe`} {
		if _, err := BuildRunValue(p); err != nil {
			t.Errorf("BuildRunValue(%q): %v", p, err)
		}
	}
}

func TestParseRunValueRoundTrip(t *testing.T) {
	exe := `C:\Users\Test User\citadel.exe`
	v, err := BuildRunValue(exe)
	if err != nil {
		t.Fatal(err)
	}
	gotExe, args, ok := ParseRunValue(v)
	if !ok || gotExe != exe || !reflect.DeepEqual(args, BackgroundWorkArgs) {
		t.Fatalf("ParseRunValue(%q) = %q %v %v", v, gotExe, args, ok)
	}
	if _, _, ok := ParseRunValue(`"C:\unterminated work`); ok {
		t.Fatal("ParseRunValue accepted an unterminated quote")
	}
	if _, _, ok := ParseRunValue(`"C:\a.exe"work`); ok {
		t.Fatal("ParseRunValue accepted arguments glued to the quote")
	}
	if e, a, ok := ParseRunValue(`C:\citadel.exe work --background`); !ok || e != `C:\citadel.exe` || len(a) != 2 {
		t.Fatalf("unquoted form = %q %v %v", e, a, ok)
	}
}

func TestAutostartStatusPointsAtThis(t *testing.T) {
	this := `C:\Users\Test User\AppData\Local\Citadel\citadel.exe`
	v, _ := BuildRunValue(this)
	cases := []struct {
		name  string
		set   bool
		value string
		want  bool
	}{
		{"absent", false, "", false},
		{"this exe", true, v, true},
		{"case and separators differ", true, `"c:/users/test user/appdata/local/citadel/CITADEL.EXE" work --background`, true},
		{"other exe", true, `"C:\Old\citadel.exe" work --background`, false},
		{"other args", true, `"` + this + `" work`, false},
		{"garbage", true, `"unterminated`, false},
	}
	for _, tc := range cases {
		st := autostartStatusFor(tc.set, tc.value, this)
		if st.Set != tc.set || st.PointsAtThis != tc.want {
			t.Errorf("%s: status = %+v, want set=%v points=%v", tc.name, st, tc.set, tc.want)
		}
	}
}

func TestSupervisorLockIsSingleInstance(t *testing.T) {
	dir := t.TempDir()
	path := SupervisorLockPath(dir)
	first, err := worklock.AcquireFile(path, "test", nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := worklock.AcquireFile(path, "test", nil)
	var running *worklock.ErrAlreadyRunning
	if !errors.As(err, &running) {
		if second != nil {
			second.Release()
		}
		t.Fatalf("second acquire err = %v, want ErrAlreadyRunning", err)
	}
	// The supervisor lock is a different file from the worker lock, so the
	// supervised worker can still take its own lock.
	worker, err := worklock.Acquire(dir+"/network", "test", nil)
	if err != nil {
		t.Fatalf("worker lock beside the supervisor lock: %v", err)
	}
	worker.Release()
	first.Release()
	again, err := worklock.AcquireFile(path, "test", nil)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again.Release()
}
