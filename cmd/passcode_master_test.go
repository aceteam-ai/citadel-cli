package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

func TestEnrollPrecheck(t *testing.T) {
	cases := []struct {
		name          string
		vaultConfig   bool
		hasLegacyHash bool
		wantCleanup   bool
		wantErr       error
	}{
		{"fresh node", false, false, false, nil},
		{"fresh node with legacy passcode", false, true, false, nil},
		{"already enrolled, clean", true, false, false, errAlreadyEnrolled},
		{"interrupted migration (vault set, legacy lingers)", true, true, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cleanup, err := enrollPrecheck(c.vaultConfig, c.hasLegacyHash)
			if cleanup != c.wantCleanup {
				t.Errorf("cleanup=%v want %v", cleanup, c.wantCleanup)
			}
			if !errors.Is(err, c.wantErr) {
				t.Errorf("err=%v want %v", err, c.wantErr)
			}
		})
	}
}

// TestLegacyOwnershipGate pins the citadel#808 fast-follow fail-closed rule:
// enroll may only trust the legacy-passcode scan when it read every candidate
// ConfigDir cleanly. Inconclusive refuses REGARDLESS of Found — an earlier
// version of this gate only checked Inconclusive when nothing was found,
// which let an invoker who could write ONE readable candidate (e.g. their own
// ~/.citadel-cli) plant a hash of a PIN they know, making Found()==true, while
// the REAL hash sat in an unreadable candidate. Only the operator's explicit
// --assume-no-legacy-passcode override may stand in for a clean scan.
func TestLegacyOwnershipGate(t *testing.T) {
	cases := []struct {
		name       string
		legacy     config.LegacyPasscodeSearchResult
		assumeNone bool
		wantErr    error
	}{
		{
			name:   "clean scan, nothing found",
			legacy: config.LegacyPasscodeSearchResult{},
		},
		{
			name:   "found a hash, clean scan",
			legacy: config.LegacyPasscodeSearchResult{Dirs: []string{"/a"}, Hashes: []string{"$2a$hash"}},
		},
		{
			name:    "inconclusive, nothing found, no override",
			legacy:  config.LegacyPasscodeSearchResult{Inconclusive: true},
			wantErr: errLegacyCheckInconclusive,
		},
		{
			name:       "inconclusive, nothing found, operator override",
			legacy:     config.LegacyPasscodeSearchResult{Inconclusive: true},
			assumeNone: true,
		},
		{
			// The exact bypass this gate exists to close: a hash WAS found
			// (e.g. planted by the invoker in their own writable candidate),
			// but the scan couldn't rule out a different, real hash living in
			// an unreadable candidate. Must refuse, not treat Found as enough.
			name: "found a hash but scan was also inconclusive elsewhere: must still refuse",
			legacy: config.LegacyPasscodeSearchResult{
				Dirs: []string{"/a"}, Hashes: []string{"$2a$hash"}, Inconclusive: true,
			},
			wantErr: errLegacyCheckInconclusive,
		},
		{
			name: "found a hash, inconclusive elsewhere, operator override",
			legacy: config.LegacyPasscodeSearchResult{
				Dirs: []string{"/a"}, Hashes: []string{"$2a$hash"}, Inconclusive: true,
			},
			assumeNone: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := legacyOwnershipGate(c.legacy, c.assumeNone)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("err=%v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err=%v, want %v", err, c.wantErr)
			}
		})
	}
}

// TestLegacyWrongPINMessageNamesDivergentDirs covers the dead-end
// Verify's AND semantics could otherwise create: two READABLE candidates
// (Inconclusive==false, so the ownership gate passes) holding hashes for two
// DIFFERENT pins can never both be satisfied by one PIN, so plain "incorrect"
// would leave the operator with no way forward. The error must name the
// directories so the state is fixable (remove the stale permissions.yaml)
// instead of a permanent enrollment dead end.
func TestLegacyWrongPINMessageNamesDivergentDirs(t *testing.T) {
	single := config.LegacyPasscodeSearchResult{Dirs: []string{"/a"}, Hashes: []string{"$2a$hash"}}
	if got := legacyWrongPINMessage(single); got != "current node passcode is incorrect" {
		t.Errorf("single-dir message = %q, want the plain incorrect-passcode message", got)
	}

	multi := config.LegacyPasscodeSearchResult{
		Dirs:   []string{"/home/alice/.citadel-cli", "/etc/citadel"},
		Hashes: []string{"$2a$hashA", "$2a$hashB"},
	}
	got := legacyWrongPINMessage(multi)
	for _, dir := range multi.Dirs {
		if !strings.Contains(got, dir) {
			t.Errorf("multi-dir message %q does not name directory %q", got, dir)
		}
	}
}

// TestPlantedHashCannotSubstituteForRealOwnershipProof is the concrete
// end-to-end regression test for the bypass described above: an invoker who
// can write to ONE candidate directory (dirA, standing in for e.g. their own
// ~/.citadel-cli) plants a hash of a PIN they know. A second candidate
// (dirB, standing in for e.g. root-owned /etc/citadel) holds the REAL legacy
// hash but is unreadable to this invoker. The scan-then-gate pipeline
// (FindLegacyPasscode -> legacyOwnershipGate, exactly as runPasscodeEnroll
// calls them) must refuse before ever reaching legacy.Verify, so the
// planted PIN can never be accepted as proof.
func TestPlantedHashCannotSubstituteForRealOwnershipProof(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied simulation doesn't work as root")
	}
	dirA, dirB := t.TempDir(), t.TempDir() // dirA: attacker-writable; dirB: real, unreadable

	plantedPIN := "1234"
	permsA := config.DefaultPermissions()
	if err := permsA.SetPasscode(plantedPIN); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(dirA, permsA); err != nil {
		t.Fatal(err)
	}

	realPIN := "9999"
	permsB := config.DefaultPermissions()
	if err := permsB.SetPasscode(realPIN); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(dirB, permsB); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dirB, "permissions.yaml"), 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dirB, "permissions.yaml"), 0600)

	legacy := config.FindLegacyPasscode([]string{dirA, dirB})
	if !legacy.Found() {
		t.Fatal("expected the planted hash in dirA to be found")
	}
	if !legacy.Inconclusive {
		t.Fatal("expected dirB's unreadable file to make the scan inconclusive")
	}

	// This is the actual security property: the gate must refuse, so
	// runPasscodeEnroll never reaches the point of asking for a passcode and
	// checking it against legacy.Verify (which — since dirB's hash was never
	// read — would otherwise accept the planted PIN via AND-over-what-was-
	// found degenerating to a single hash).
	if err := legacyOwnershipGate(legacy, false /* no override */); !errors.Is(err, errLegacyCheckInconclusive) {
		t.Fatalf("legacyOwnershipGate = %v, want errLegacyCheckInconclusive (planted hash must not substitute for the real proof)", err)
	}
}

// TestClearLegacyPasscodeAtClearsEveryDivergentDir is the write-side
// complement of the ownership-check fix: once a legacy hash is proven and
// enrollment proceeds, the hash must be deleted from EVERY directory it was
// found in, not just the current invocation's platform.ConfigDir() (else the
// exact brute-force target enrollment exists to delete would survive
// enrollment under whichever directory the check didn't clear).
func TestClearLegacyPasscodeAtClearsEveryDivergentDir(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	for _, dir := range []string{dirA, dirB} {
		perms := config.DefaultPermissions()
		if err := perms.SetPasscode("1379"); err != nil {
			t.Fatal(err)
		}
		if err := config.SavePermissions(dir, perms); err != nil {
			t.Fatal(err)
		}
	}

	if err := clearLegacyPasscodeAt([]string{dirA, dirB}); err != nil {
		t.Fatalf("clearLegacyPasscodeAt: %v", err)
	}

	for _, dir := range []string{dirA, dirB} {
		perms := config.LoadPermissions(dir)
		if perms.PasscodeHash != "" {
			t.Errorf("legacy hash still present in %s after clear", dir)
		}
	}
}

func TestClearLegacyPasscodeAtIsIdempotentOnEmptyDirs(t *testing.T) {
	// A fresh node with nothing to clear: no file exists yet in either dir.
	// This must be a harmless no-op, matching the pre-fix behavior of
	// unconditionally touching platform.ConfigDir() even when there's no
	// legacy hash to delete.
	dir := t.TempDir()
	if err := clearLegacyPasscodeAt([]string{dir}); err != nil {
		t.Fatalf("clearLegacyPasscodeAt on empty dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "permissions.yaml")); err != nil {
		t.Fatalf("expected permissions.yaml to be created (pre-fix behavior preserved): %v", err)
	}
}

func TestLegacyClearDirsDeduplicatesAndIncludesConfigDir(t *testing.T) {
	legacy := config.LegacyPasscodeSearchResult{Dirs: []string{platform.ConfigDir(), "/some/other/dir"}}
	got := legacyClearDirs(legacy)
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d] {
			t.Fatalf("duplicate directory %q in %v", d, got)
		}
		seen[d] = true
	}
	if !seen[platform.ConfigDir()] {
		t.Fatalf("legacyClearDirs(%v) = %v, missing platform.ConfigDir()", legacy, got)
	}
	if !seen["/some/other/dir"] {
		t.Fatalf("legacyClearDirs(%v) = %v, missing the discovered legacy dir", legacy, got)
	}
}
