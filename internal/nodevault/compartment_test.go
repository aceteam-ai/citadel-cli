package nodevault

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestKeyCompartmentRoundTripAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "sessions.key")
	s1, err := OpenKeyCompartment(path, true)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := s1.Seal("sessions/a", []byte("tok"))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := OpenKeyCompartment(path, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Unseal("sessions/a", sealed)
	if err != nil || string(got) != "tok" {
		t.Fatalf("reopen unseal = %q, %v", got, err)
	}
	if _, err := s2.Unseal("sessions/b", sealed); err == nil {
		t.Fatal("context mismatch must fail")
	}
}

func TestKeyCompartmentRefusesMissingWhenNotCreating(t *testing.T) {
	_, err := OpenKeyCompartment(filepath.Join(t.TempDir(), "k"), false)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeyCompartmentRefusesLooseOrShortKey(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenKeyCompartment(short, true); !errors.Is(err, ErrCompartmentKeyUnsafe) {
		t.Fatalf("short key err = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenKeyCompartment(loose, true); !errors.Is(err, ErrCompartmentKeyUnsafe) {
		t.Fatalf("loose key err = %v", err)
	}
}

func TestKeyCompartmentRefusesRelativePath(t *testing.T) {
	if _, err := OpenKeyCompartment("rel.key", true); err == nil {
		t.Fatal("relative path must be refused")
	}
}
