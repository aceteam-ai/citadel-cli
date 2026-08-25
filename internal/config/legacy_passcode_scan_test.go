package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writePermissionsWithHash(t *testing.T, dir, pin string) {
	t.Helper()
	p := DefaultPermissions()
	if err := p.SetPasscode(pin); err != nil {
		t.Fatalf("SetPasscode: %v", err)
	}
	if err := SavePermissions(dir, p); err != nil {
		t.Fatalf("SavePermissions: %v", err)
	}
}

func TestFindLegacyPasscode_NoneFound(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir()}
	result := FindLegacyPasscode(dirs)
	if result.Found() {
		t.Fatalf("Found()=true, want false: %+v", result)
	}
	if result.Inconclusive {
		t.Fatal("Inconclusive=true for two simply-empty candidates")
	}
}

func TestFindLegacyPasscode_FoundInOneCandidate(t *testing.T) {
	empty := t.TempDir()
	withHash := t.TempDir()
	writePermissionsWithHash(t, withHash, "1379")

	result := FindLegacyPasscode([]string{empty, withHash})
	if !result.Found() {
		t.Fatal("Found()=false, want true")
	}
	if result.Inconclusive {
		t.Fatal("Inconclusive=true despite a clean scan")
	}
	if len(result.Dirs) != 1 || result.Dirs[0] != withHash {
		t.Fatalf("Dirs=%v, want [%s]", result.Dirs, withHash)
	}
	if !result.Verify("1379") {
		t.Fatal("Verify(correct pin) = false")
	}
	if result.Verify("0000") {
		t.Fatal("Verify(wrong pin) = true")
	}
	if result.Verify("") {
		t.Fatal("Verify(empty pin) = true")
	}
}

// TestFindLegacyPasscode_FoundAcrossDivergentContexts is the exact scenario
// citadel#808's enroll ownership-bypass depended on: the legacy passcode was
// written under ONE ConfigDir (e.g. a systemd-root citadel work) and enroll
// is now scanning a candidate list that includes a DIFFERENT current
// invocation's ConfigDir (e.g. an interactive non-root shell) as well as the
// one that actually has the hash. The scan must not miss it just because the
// first candidate came up empty.
func TestFindLegacyPasscode_FoundAcrossDivergentContexts(t *testing.T) {
	interactiveInvokerDir := t.TempDir() // e.g. platform.ConfigDir() for a non-root shell
	systemdWorkerDir := t.TempDir()      // e.g. platform.ConfigDir() for HOME=/root
	writePermissionsWithHash(t, systemdWorkerDir, "9137")

	// Simulate scanning candidates in the order ConfigDirCandidates() would
	// produce: current invocation first, other plausible contexts after.
	result := FindLegacyPasscode([]string{interactiveInvokerDir, systemdWorkerDir})
	if !result.Found() {
		t.Fatal("legacy hash written under a different invocation context was not found")
	}
	if !result.Verify("9137") {
		t.Fatal("Verify against the hash found in the divergent context failed")
	}
}

func TestFindLegacyPasscode_InconclusiveOnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied simulation doesn't work as root")
	}
	dir := t.TempDir()
	writePermissionsWithHash(t, dir, "1379")
	path := filepath.Join(dir, permissionsFile)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0600)

	result := FindLegacyPasscode([]string{dir})
	if result.Found() {
		t.Fatal("Found()=true for a file that could not be read")
	}
	if !result.Inconclusive {
		t.Fatal("Inconclusive=false despite an unreadable candidate; this must fail closed, not read as 'no passcode'")
	}
}

func TestFindLegacyPasscode_InconclusiveOnMalformedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, permissionsFile), []byte("not: [valid yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	result := FindLegacyPasscode([]string{dir})
	if result.Found() {
		t.Fatal("Found()=true for malformed YAML")
	}
	if !result.Inconclusive {
		t.Fatal("Inconclusive=false for malformed YAML; a file that couldn't be parsed must not read as 'no passcode'")
	}
}

// TestFindLegacyPasscode_SamePINWrittenTwiceStillVerifies covers the
// legitimate case behind AND-semantics: the same real passcode was written
// under two candidate directories (e.g. the operator ran 'citadel passcode
// set' both interactively and via an earlier root/systemd context). Both
// hashes are for the same PIN, so requiring a match against every found hash
// still succeeds.
func TestFindLegacyPasscode_SamePINWrittenTwiceStillVerifies(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writePermissionsWithHash(t, dirA, "1379")
	writePermissionsWithHash(t, dirB, "1379")

	result := FindLegacyPasscode([]string{dirA, dirB})
	if len(result.Dirs) != 2 {
		t.Fatalf("Dirs=%v, want 2 entries", result.Dirs)
	}
	if !result.Verify("1379") {
		t.Fatal("Verify must match when the pin satisfies every found hash")
	}
	if result.Verify("0000") {
		t.Fatal("Verify matched a wrong pin")
	}
}

// TestFindLegacyPasscode_DivergentHashesNeverVerify pins Verify's AND
// semantics: a hash an unprivileged invoker could plant in a writable
// candidate (dirA) must never let them "prove ownership" merely because a
// DIFFERENT, real hash also exists elsewhere (dirB) — neither pin alone
// satisfies both hashes, so Verify must reject both. This is the
// defense-in-depth complement to legacyOwnershipGate's Inconclusive check in
// cmd (see TestPlantedHashCannotSubstituteForRealOwnershipProof there): even
// if a scan somehow read every candidate cleanly (Inconclusive==false) while
// still containing an extra, foreign hash, OR-semantics would have let that
// foreign hash's own PIN stand in as proof. AND-semantics means it can only
// ever add a requirement, never substitute for the real one.
func TestFindLegacyPasscode_DivergentHashesNeverVerify(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	writePermissionsWithHash(t, dirA, "1234") // planted
	writePermissionsWithHash(t, dirB, "9999") // real

	result := FindLegacyPasscode([]string{dirA, dirB})
	if result.Verify("1234") {
		t.Fatal("the planted PIN must not verify merely because a different real hash also exists")
	}
	if result.Verify("9999") {
		t.Fatal("the real PIN alone must not verify either -- it doesn't satisfy the planted hash")
	}
}
