//go:build linux || darwin

package harnessseat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRegistryStoreNeverRegeneratesSaltForExistingSeats(t *testing.T) {
	s, dir := saltFixture(t)
	raw, resolve := registryFixture(t)
	path := filepath.Join(dir, registryFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitializeRegistry(context.Background(), resolve); !errors.Is(err, ErrSaltMissing) {
		t.Fatalf("missing salt for existing seats accepted: %v", err)
	}
	if _, err := s.InitializeSalt(context.Background()); !errors.Is(err, ErrSaltMissing) {
		t.Fatalf("generic salt initialization bypassed registry check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, saltFileName)); !os.IsNotExist(err) {
		t.Fatal("regenerated existing identity")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
		t.Fatal("existing declaration changed")
	}
}

func TestRegistryStoreReadDoesNotInitializeAndEmptyProfilesCanInitialize(t *testing.T) {
	s, dir := saltFixture(t)
	if _, err := s.LoadRegistry(context.Background(), nil); !errors.Is(err, ErrRegistryMissing) {
		t.Fatal("missing registry accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, saltFileName)); !os.IsNotExist(err) {
		t.Fatal("read created identity")
	}
	path := filepath.Join(dir, registryFileName)
	raw := []byte("profiles: {fixture-profile: {enabled: false}}\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRegistry(context.Background(), nil); !errors.Is(err, ErrSaltMissing) {
		t.Fatal("read initialized salt")
	}
	first, err := s.InitializeRegistry(context.Background(), nil)
	if err != nil || len(first.Seats()) != 0 {
		t.Fatal("empty profile registry could not initialize")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(raw, got) {
		t.Fatal("initialization rewrote existing profiles")
	}
	before, err := s.LoadSalt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitializeRegistry(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	after, err := s.LoadSalt(context.Background())
	if err != nil || before != after {
		t.Fatal("repeated initialization rotated identity")
	}
}

func TestRegistryStorePreservesProfilesAndRefusesStaleUpdate(t *testing.T) {
	s, dir := saltFixture(t)
	raw, resolve := registryFixture(t)
	if _, err := s.InitializeSalt(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, registryFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := s.LoadRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	seats := first.Seats()
	seats[0].Label = "Updated"
	second, err := s.UpdateRegistry(context.Background(), first, seats)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision() == first.Revision() || second.Seats()[0].Label != "Updated" {
		t.Fatal("update was not published")
	}
	if _, err := s.UpdateRegistry(context.Background(), first, nil); !errors.Is(err, ErrRegistryConflict) {
		t.Fatalf("stale snapshot overwrote current state: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(content, []byte("fixture-profile")) {
		t.Fatal("profile section lost")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("private mode lost")
	}
}

func TestRegistryStoreConcurrentWritersCannotLoseUpdate(t *testing.T) {
	s, dir := saltFixture(t)
	_, resolve := registryFixture(t)
	first, err := s.InitializeRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenSaltStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second, err := other.LoadRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := resolve("fixture-user")
	seat := Seat{Identity: Identity{ID: "first-seat", Harness: Codex, Owner: "fixture-user", HomeDir: home, ConfigDir: filepath.Join(home, "seat-one")}, Label: "First", Entitlement: "consumer", DataCeiling: "public", Auth: AuthUnknown}
	seat2 := seat
	seat2.ID, seat2.ConfigDir = "other-seat", filepath.Join(home, "seat-two")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = s.UpdateRegistry(context.Background(), first, []Seat{seat}) }()
	go func() {
		defer wg.Done()
		_, errs[1] = other.UpdateRegistry(context.Background(), second, []Seat{seat2})
	}()
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrRegistryConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("writers were not fenced: %v", errs)
	}
	current, err := s.LoadRegistry(context.Background(), resolve)
	if err != nil || len(current.Seats()) != 1 {
		t.Fatal("published declaration lost")
	}
}

func TestRegistryStoreRefusesUnsafeFilesWithoutRepair(t *testing.T) {
	for _, kind := range []string{"public", "symlink", "hardlink", "directory", "oversized", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			s, dir := saltFixture(t)
			if _, err := s.InitializeSalt(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, registryFileName)
			raw := []byte("seats: []\n")
			switch kind {
			case "symlink", "hardlink":
				target := filepath.Join(t.TempDir(), "fixture.yaml")
				if err := os.WriteFile(target, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(target, path)
				} else {
					err = os.Link(target, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			default:
				if kind == "oversized" {
					raw = bytes.Repeat([]byte("x"), MaxRegistryBytes+1)
				}
				if kind == "malformed" {
					raw = []byte("seats: true\n")
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if kind == "public" {
					if err := os.Chmod(path, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.LoadRegistry(context.Background(), nil); err == nil {
				t.Fatal("unsafe read accepted")
			}
			if _, err := s.InitializeRegistry(context.Background(), nil); err == nil {
				t.Fatal("unsafe initialization accepted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("unsafe declaration repaired or replaced")
			}
		})
	}
}

func TestRegistryStoreFencesSnapshotsSaltAndCancellation(t *testing.T) {
	s, dir := saltFixture(t)
	first, err := s.InitializeRegistry(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenSaltStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.UpdateRegistry(context.Background(), first, nil); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("foreign snapshot accepted")
	}
	if _, err := s.UpdateRegistry(context.Background(), RegistrySnapshot{}, nil); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("zero snapshot accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.UpdateRegistry(canceled, first, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled write accepted")
	}
	path := filepath.Join(dir, registryFileName)
	before, _ := os.ReadFile(path)
	if err := os.WriteFile(filepath.Join(dir, saltFileName), bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRegistry(context.Background(), first, nil); !errors.Is(err, ErrRegistryConflict) {
		t.Fatal("changed salt accepted")
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(before, got) {
		t.Fatal("refused write changed registry")
	}
}

func TestRegistryStoreReloadsTrustedOwnerAndPreservesPrivateSnapshot(t *testing.T) {
	s, dir := saltFixture(t)
	raw, resolve := registryFixture(t)
	if _, err := s.InitializeSalt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, registryFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := s.LoadRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	seats := first.Seats()
	seats[0].HomeDir = t.TempDir()
	second, err := s.UpdateRegistry(context.Background(), first, seats)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := resolve("fixture-user")
	if second.Seats()[0].HomeDir != home {
		t.Fatal("caller supplied home overrode trusted owner")
	}
	data, err := json.Marshal(second)
	if err != nil || string(data) != "{}" {
		t.Fatal("private snapshot escaped JSON")
	}
}

func TestRegistryStoreRemovingSeatLeavesConfigDirectoryUntouched(t *testing.T) {
	s, dir := saltFixture(t)
	raw, resolve := registryFixture(t)
	doc, err := DecodeRegistry(raw, resolve)
	if err != nil {
		t.Fatal(err)
	}
	configDir := doc.Seats()[0].ConfigDir
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(configDir, "untouched.fixture")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitializeSalt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, registryFileName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := s.LoadRegistry(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.UpdateRegistry(context.Background(), before, nil)
	if err != nil || len(after.Seats()) != 0 {
		t.Fatal("seat removal failed")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatal("remove touched vendor configuration")
	}
}
