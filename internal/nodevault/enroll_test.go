package nodevault

import (
	"errors"
	"testing"
)

func TestEnrollFreshNode(t *testing.T) {
	v := newTestVault(t)
	// No legacy passcode: legacyVerify nil, deleteLegacy nil.
	if err := v.Enroll("", "654321", DefaultPolicy(), true, nil, nil); err != nil {
		t.Fatalf("enroll fresh: %v", err)
	}
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("verify after enroll: %v", err)
	}
}

func TestEnrollProvesLegacyAndDeletesHash(t *testing.T) {
	v := newTestVault(t)

	legacyDeleted := false
	legacyVerify := func(pin string) bool { return pin == "1379" } // legacy 4-digit
	deleteLegacy := func() error { legacyDeleted = true; return nil }

	// Legacy 4-digit proven; new master PIN is policy-compliant 6-digit.
	if err := v.Enroll("1379", "654321", DefaultPolicy(), true, legacyVerify, deleteLegacy); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !legacyDeleted {
		t.Fatal("legacy hash was not deleted after enrollment")
	}
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("verify new master PIN: %v", err)
	}
}

func TestEnrollWrongLegacyPINRejected(t *testing.T) {
	v := newTestVault(t)
	deleted := false
	err := v.Enroll("9999", "654321", DefaultPolicy(), true,
		func(string) bool { return false }, // legacy proof fails
		func() error { deleted = true; return nil },
	)
	if !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("want ErrWrongPIN, got %v", err)
	}
	if v.IsConfigured() {
		t.Fatal("vault set up despite failed legacy proof")
	}
	if deleted {
		t.Fatal("legacy hash deleted despite failed proof")
	}
}

func TestEnrollNewPINPolicyEnforced(t *testing.T) {
	v := newTestVault(t)
	// Legacy proof would pass, but the NEW pin is too short.
	err := v.Enroll("1379", "12", DefaultPolicy(), true,
		func(string) bool { return true },
		func() error { return nil },
	)
	if err == nil {
		t.Fatal("short new PIN accepted at enroll")
	}
	if v.IsConfigured() {
		t.Fatal("vault set up with a policy-violating PIN")
	}
}

// TestEnrollIdempotentAfterDeleteFailure covers the migration failure mode: the
// vault is written but deleting the legacy hash fails. Re-running Enroll must
// NOT re-verify/re-set (the vault exists) and must retry the delete to finish.
func TestEnrollIdempotentAfterDeleteFailure(t *testing.T) {
	v := newTestVault(t)

	failing := true
	deleteLegacy := func() error {
		if failing {
			return errors.New("disk full")
		}
		return nil
	}
	legacyVerify := func(pin string) bool { return pin == "1379" }

	// First attempt: vault gets written, delete fails.
	if err := v.Enroll("1379", "654321", DefaultPolicy(), true, legacyVerify, deleteLegacy); err == nil {
		t.Fatal("expected delete failure on first enroll")
	}
	if !v.IsConfigured() {
		t.Fatal("vault should be durably written even though delete failed")
	}

	// Second attempt with a legacyVerify that would now REJECT: idempotent path
	// must not call it (vault already configured), and the delete now succeeds.
	failing = false
	if err := v.Enroll("wrong", "different", DefaultPolicy(), true,
		func(string) bool { return false }, deleteLegacy); err != nil {
		t.Fatalf("idempotent re-enroll: %v", err)
	}
	// The master PIN from the FIRST enroll is intact (second didn't re-set).
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("original master PIN broken by idempotent re-run: %v", err)
	}
}
