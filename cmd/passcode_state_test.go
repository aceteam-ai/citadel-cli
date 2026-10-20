package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/jobs"
	"github.com/aceteam-ai/citadel-cli/internal/nodevault"
)

func TestPasscodeStateFromFailsClosedOnUnreadableState(t *testing.T) {
	// No permissions file and no vault: no passcode set.
	permsDir := t.TempDir()
	st, err := passcodeStateFrom(permsDir, nodevault.Open(t.TempDir()))
	if err != nil || st.Set {
		t.Fatalf("fresh node: state = %+v, %v; want unset, nil", st, err)
	}

	// A legacy passcode hash is reported as set and verifies.
	p := config.DefaultPermissions()
	if err := p.SetPasscode("4821-pass"); err != nil {
		t.Fatal(err)
	}
	if err := config.SavePermissions(permsDir, p); err != nil {
		t.Fatal(err)
	}
	st, err = passcodeStateFrom(permsDir, nodevault.Open(t.TempDir()))
	if err != nil || !st.Set || st.Check("4821-pass") != jobs.PasscodeMatch || st.Check("wrong") != jobs.PasscodeMismatch {
		t.Fatalf("legacy passcode: state = %+v, %v", st, err)
	}

	// An unparseable permissions file is an error, never "no passcode".
	badPerms := t.TempDir()
	if err := os.WriteFile(filepath.Join(badPerms, "permissions.yaml"), []byte("passcode_hash: [broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := passcodeStateFrom(badPerms, nodevault.Open(t.TempDir())); err == nil {
		t.Fatal("an unparseable permissions file must be an error")
	}

	// A vault that cannot be stat'ed is an error, never "no master PIN".
	notADir := filepath.Join(t.TempDir(), "node-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := passcodeStateFrom(t.TempDir(), nodevault.Open(notADir)); err == nil {
		t.Fatal("an unreadable vault must be an error")
	}
}

func TestVaultPasscodeVerdictReportsLockoutSeparately(t *testing.T) {
	vault := nodevault.Open(t.TempDir())
	if err := vault.SetPIN("correct-horse-battery-1", nodevault.DefaultPolicy(), true); err != nil {
		t.Fatal(err)
	}
	if got := vaultPasscodeVerdict(vault, "correct-horse-battery-1"); got != jobs.PasscodeMatch {
		t.Fatalf("correct PIN: %v", got)
	}
	if got := vaultPasscodeVerdict(vault, ""); got != jobs.PasscodeMismatch {
		t.Fatalf("empty PIN: %v", got)
	}
	locked := false
	for i := 0; i < 20 && !locked; i++ {
		locked = vaultPasscodeVerdict(vault, "wrong-pin-attempt") == jobs.PasscodeLockedOut
	}
	if !locked {
		t.Fatal("repeated wrong PINs never reached the vault lockout")
	}
	if got := vaultPasscodeVerdict(vault, "correct-horse-battery-1"); got != jobs.PasscodeLockedOut {
		t.Fatalf("a correct PIN during lockout must be PasscodeLockedOut, not a mismatch: %v", got)
	}
}

func TestPasscodeVerdictFromUnreadableStateIsUnavailable(t *testing.T) {
	badPerms := t.TempDir()
	if err := os.WriteFile(filepath.Join(badPerms, "permissions.yaml"), []byte("passcode_hash: [broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := passcodeVerdictFrom(badPerms, nodevault.Open(t.TempDir()), "anything"); got != jobs.PasscodeUnavailable {
		t.Fatalf("unparseable permissions: %v, want PasscodeUnavailable", got)
	}
	notADir := filepath.Join(t.TempDir(), "node-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := passcodeVerdictFrom(t.TempDir(), nodevault.Open(notADir), "anything"); got != jobs.PasscodeUnavailable {
		t.Fatalf("unreadable vault: %v, want PasscodeUnavailable", got)
	}
	p := config.DefaultPermissions()
	if err := p.SetPasscode("4821-pass"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := config.SavePermissions(dir, p); err != nil {
		t.Fatal(err)
	}
	if got := passcodeVerdictFrom(dir, nodevault.Open(t.TempDir()), "wrong"); got != jobs.PasscodeMismatch {
		t.Fatalf("wrong legacy passcode: %v, want PasscodeMismatch", got)
	}
}
