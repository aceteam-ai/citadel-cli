//go:build windows

package service

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

// TestRunValueRegistryRoundTrip writes, reads and deletes an HKCU Run value
// under a random test-only name. It never touches UserAutostartValueName.
func TestRunValueRegistryRoundTrip(t *testing.T) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	name := "CitadelWorkerTest-" + hex.EncodeToString(b[:])
	if name == UserAutostartValueName {
		t.Fatal("test value name collides with the real value")
	}
	t.Cleanup(func() { _ = deleteRunValue(name) })

	if _, set, err := readRunValue(name); err != nil || set {
		t.Fatalf("before write: set=%v err=%v", set, err)
	}
	exe := `C:\Users\Test User\AppData\Local\Citadel\citadel.exe`
	value, err := BuildRunValue(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeRunValue(name, value); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, set, err := readRunValue(name)
	if err != nil || !set || got != value {
		t.Fatalf("read = %q set=%v err=%v, want %q", got, set, err, value)
	}
	if st := autostartStatusFor(set, got, exe); !st.PointsAtThis {
		t.Fatalf("status = %+v, want PointsAtThis", st)
	}
	if err := deleteRunValue(name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, set, err := readRunValue(name); err != nil || set {
		t.Fatalf("after delete: set=%v err=%v", set, err)
	}
	if err := deleteRunValue(name); err != nil {
		t.Fatalf("deleting an absent value: %v", err)
	}
}
