package config

import "testing"

// resetVaultHooks restores the package hooks after a test so cases don't leak.
func resetVaultHooks(t *testing.T) {
	t.Helper()
	origCfg, origVer := VaultConfigured, VaultVerify
	t.Cleanup(func() { VaultConfigured, VaultVerify = origCfg, origVer })
}

func TestVerifyPasscodeDelegatesToVault(t *testing.T) {
	resetVaultHooks(t)
	VaultConfigured = func() bool { return true }
	VaultVerify = func(pin string) (bool, bool) { return pin == "masterpin", true }

	// Even with a stale legacy hash present, the vault answer wins when handled.
	p := &Permissions{PasscodeHash: "$2a$fake"}

	if !p.VerifyPasscode("masterpin") {
		t.Fatal("correct master PIN rejected")
	}
	if p.VerifyPasscode("legacy") {
		t.Fatal("legacy PIN accepted while vault is enrolled")
	}
	if !p.HasPasscode() {
		t.Fatal("HasPasscode false while a master PIN is enrolled")
	}
}

func TestVerifyPasscodeFallsBackWhenUnhandled(t *testing.T) {
	resetVaultHooks(t)
	// Hook present but reports "not handled" (no vault): legacy path must run.
	VaultConfigured = func() bool { return false }
	VaultVerify = func(string) (bool, bool) { return false, false }

	p := &Permissions{}
	if err := p.SetPasscode("1234"); err != nil {
		t.Fatal(err)
	}
	if !p.VerifyPasscode("1234") {
		t.Fatal("legacy passcode not honored when vault is absent")
	}
	if p.VerifyPasscode("wrong") {
		t.Fatal("wrong legacy passcode accepted")
	}
}

func TestSetPasscodeBlockedWhenVaultEnrolled(t *testing.T) {
	resetVaultHooks(t)
	VaultConfigured = func() bool { return true }

	p := &Permissions{}
	// Anti-resurrection: no fresh bcrypt hash may be written while a master PIN
	// is enrolled (that would recreate the deleted brute-force target and let a
	// platform push set the gate secret remotely).
	if err := p.SetPasscode("9999"); err == nil {
		t.Fatal("SetPasscode allowed a fresh hash while vault enrolled")
	}
	if p.PasscodeHash != "" {
		t.Fatal("a hash was written despite the guard")
	}
	// Clearing (empty pin) stays allowed — the enrollment path uses it.
	if err := p.SetPasscode(""); err != nil {
		t.Fatalf("clearing should stay allowed: %v", err)
	}
}
