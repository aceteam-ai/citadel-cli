package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultPermissions_SensitiveSurfacesOff is the core aceteam#6524 guarantee:
// a freshly joined node (no permissions.yaml) does NOT expose the sensitive
// remote-access surfaces (console/desktop/files) or shell, while the lower-stakes
// operational surfaces (services/ssh/provision) stay on.
func TestDefaultPermissions_SensitiveSurfacesOff(t *testing.T) {
	p := DefaultPermissions()
	if p.Console || p.Desktop || p.Files || p.Shell {
		t.Errorf("console/desktop/files/shell must be default-DENY on a fresh node, got %+v", p)
	}
	if !p.Services || !p.SSH || !p.Provision {
		t.Errorf("services/ssh/provision should stay default-on (opt-out), got %+v", p)
	}
	if p.HasPasscode() {
		t.Error("a fresh node must not have a passcode set")
	}
}

// TestLoadPermissions_NoFile_FreshNode asserts the on-disk-absent path returns the
// same locked-down posture (the actual code path a just-joined node hits).
func TestLoadPermissions_NoFile_FreshNode(t *testing.T) {
	dir := t.TempDir()
	p := LoadPermissions(dir)
	if p.Console || p.Desktop || p.Files || p.Shell {
		t.Errorf("no-file load must leave sensitive surfaces disabled, got %+v", p)
	}
	if !p.Services || !p.SSH || !p.Provision {
		t.Errorf("no-file load should keep services/ssh/provision on, got %+v", p)
	}
}

// TestLoadPermissions_AbsentKeysStayDisabled verifies a partial/legacy config
// that omits console/desktop/files leaves them DISABLED (fail closed), so a
// permissions.yaml written before this change does not silently re-open them.
func TestLoadPermissions_AbsentKeysStayDisabled(t *testing.T) {
	dir := t.TempDir()
	// Legacy-shaped file that only sets ssh; the sensitive keys are absent.
	data := []byte("ssh: true\n")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	p := LoadPermissions(dir)
	if p.Console || p.Desktop || p.Files {
		t.Errorf("absent sensitive keys must default to DISABLED, got %+v", p)
	}
}

// TestLoadPermissions_ExplicitOptInRoundTrips confirms an operator's opt-in
// persists and reloads.
func TestLoadPermissions_ExplicitOptInRoundTrips(t *testing.T) {
	dir := t.TempDir()
	data := []byte("console: true\ndesktop: true\nfiles: true\n")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	p := LoadPermissions(dir)
	if !p.Console || !p.Desktop || !p.Files {
		t.Errorf("explicit opt-in should round-trip true, got %+v", p)
	}
}

// TestPasscode_SetVerifyHashesNotPlaintext verifies the passcode is stored
// hashed (never plaintext) and verifies correctly.
func TestPasscode_SetVerifyHashesNotPlaintext(t *testing.T) {
	p := DefaultPermissions()
	if err := p.SetPasscode("1379"); err != nil {
		t.Fatalf("SetPasscode: %v", err)
	}
	if p.PasscodeHash == "" {
		t.Fatal("passcode hash should be set")
	}
	if p.PasscodeHash == "1379" {
		t.Fatal("passcode must NOT be stored in plaintext")
	}
	if !p.HasPasscode() {
		t.Error("HasPasscode should be true after SetPasscode")
	}
	if !p.VerifyPasscode("1379") {
		t.Error("correct passcode should verify")
	}
	if p.VerifyPasscode("0000") {
		t.Error("wrong passcode must be rejected")
	}
}

// TestPasscode_FailsClosed is the "enabled != open" guarantee: verification with
// no passcode set, or an empty pin, must fail — so an enabled-but-passcode-less
// surface stays locked.
func TestPasscode_FailsClosed(t *testing.T) {
	p := DefaultPermissions() // no passcode
	if p.VerifyPasscode("anything") {
		t.Error("verify must fail closed when no passcode is set")
	}
	if err := p.SetPasscode("1379"); err != nil {
		t.Fatalf("SetPasscode: %v", err)
	}
	if p.VerifyPasscode("") {
		t.Error("an empty pin must never verify")
	}
	// Clearing the passcode re-locks.
	if err := p.SetPasscode(""); err != nil {
		t.Fatalf("clear passcode: %v", err)
	}
	if p.HasPasscode() || p.VerifyPasscode("1379") {
		t.Error("clearing the passcode should re-lock (HasPasscode false, verify fails)")
	}
}

// TestPasscode_RoundTripsThroughSaveLoad confirms the hash persists and still
// verifies after a save/load cycle, and that a leaked file never carries the PIN.
func TestPasscode_RoundTripsThroughSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := DefaultPermissions()
	p.Console = true
	if err := p.SetPasscode("2468"); err != nil {
		t.Fatalf("SetPasscode: %v", err)
	}
	if err := SavePermissions(dir, p); err != nil {
		t.Fatalf("SavePermissions: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "permissions.yaml"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if containsPlaintextPIN(string(raw), "2468") {
		t.Error("persisted permissions.yaml must not contain the plaintext PIN")
	}

	loaded := LoadPermissions(dir)
	if !loaded.Console {
		t.Error("console opt-in should persist")
	}
	if !loaded.VerifyPasscode("2468") {
		t.Error("passcode should verify after save/load round-trip")
	}
}

// TestIsSensitiveCategory pins the set of passcode-gated surfaces the gateway and
// listener paths agree on.
func TestIsSensitiveCategory(t *testing.T) {
	for _, c := range []string{"console", "desktop", "files"} {
		if !IsSensitiveCategory(c) {
			t.Errorf("%q should be a sensitive category", c)
		}
	}
	for _, c := range []string{"services", "ssh", "provision", "shell", ""} {
		if IsSensitiveCategory(c) {
			t.Errorf("%q should NOT be a sensitive category", c)
		}
	}
}

// TestSavePermissions_FileMode0600 confirms the file carrying the passcode hash
// is not group/world-readable.
func TestSavePermissions_FileMode0600(t *testing.T) {
	dir := t.TempDir()
	p := DefaultPermissions()
	_ = p.SetPasscode("1379")
	if err := SavePermissions(dir, p); err != nil {
		t.Fatalf("SavePermissions: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "permissions.yaml"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("permissions.yaml should be 0600 (holds a credential), got %o", perm)
	}
}

func TestLoadPermissions_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	data := []byte("not: [valid: yaml: {{{")
	if err := os.WriteFile(filepath.Join(dir, "permissions.yaml"), data, 0600); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}
	p := LoadPermissions(dir)
	// Should still return the (locked-down) defaults on parse error.
	if p.Console || p.Desktop || p.Files {
		t.Errorf("invalid YAML should return locked-down defaults, got %+v", p)
	}
}

// containsPlaintextPIN reports whether s contains the raw pin (used to prove the
// stored bcrypt hash is not the plaintext).
func containsPlaintextPIN(s, pin string) bool {
	for i := 0; i+len(pin) <= len(s); i++ {
		if s[i:i+len(pin)] == pin {
			return true
		}
	}
	return false
}
