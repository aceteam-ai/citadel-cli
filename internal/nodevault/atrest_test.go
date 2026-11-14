package nodevault

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// strongPassphrase is a synthetic four-word passphrase well above 60 bits.
const strongPassphrase = "lantern orbit pebble saffron"

// TestAtRestRefusesShortPIN is #820's acceptance: a vault set with a 6-digit
// access PIN still opens the access gate, but no at-rest consumer can use it.
func TestAtRestRefusesShortPIN(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")

	// Access gate unchanged.
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("access PIN must still verify: %v", err)
	}
	// At-rest refused, even with the correct PIN.
	if _, err := v.UnlockAtRest("654321", DefaultPolicy()); !errors.Is(err, ErrWeakSecret) {
		t.Fatalf("UnlockAtRest with a 6-digit PIN: want ErrWeakSecret, got %v", err)
	}
}

func TestSetPassphraseRefusesWeakAndAcceptsStrong(t *testing.T) {
	v := newTestVault(t)
	for _, weak := range []string{"654321", "correct", "abcdefgh1"} {
		if err := v.SetPassphrase(weak, DefaultPolicy(), true); !errors.Is(err, ErrWeakSecret) {
			t.Fatalf("SetPassphrase(%q): want ErrWeakSecret, got %v", weak, err)
		}
	}
	if v.IsConfigured() {
		t.Fatal("a refused passphrase must not create the vault")
	}
	if err := v.SetPassphrase(strongPassphrase, DefaultPolicy(), false); !errors.Is(err, ErrAckRequired) {
		t.Fatalf("missing ack: want ErrAckRequired, got %v", err)
	}
	if err := v.SetPassphrase(strongPassphrase, DefaultPolicy(), true); err != nil {
		t.Fatalf("SetPassphrase strong: %v", err)
	}
	sess, err := v.UnlockAtRest(strongPassphrase, DefaultPolicy())
	if err != nil {
		t.Fatalf("UnlockAtRest strong: %v", err)
	}
	sess.Lock()
	if _, err := v.UnlockAtRest(strongPassphrase+"x", DefaultPolicy()); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("wrong strong passphrase: want ErrWrongPIN, got %v", err)
	}
}

// A policy file cannot lower the at-rest bar below the default threshold.
func TestAtRestThresholdHasFloor(t *testing.T) {
	low := DefaultPolicy()
	low.E2EThresholdBits = 10
	if got := AtRestThresholdBits(low); got != DefaultE2EThresholdBits {
		t.Fatalf("AtRestThresholdBits with a lowered policy = %v, want %v", got, DefaultE2EThresholdBits)
	}
	high := DefaultPolicy()
	high.E2EThresholdBits = 90
	if got := AtRestThresholdBits(high); got != 90 {
		t.Fatalf("AtRestThresholdBits with a raised policy = %v, want 90", got)
	}
	if err := ValidateAtRest("654321", low); !errors.Is(err, ErrWeakSecret) {
		t.Fatalf("lowered policy must not admit a PIN: %v", err)
	}
}

// Weak at-rest attempts are refused before the KDF and never counted toward
// lockout, so free short guesses cannot lock the vault.
func TestUnlockAtRestWeakNotCountedTowardLockout(t *testing.T) {
	v := newTestVault(t)
	if err := v.SetPassphrase(strongPassphrase, DefaultPolicy(), true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxFreeAttempts+3; i++ {
		if _, err := v.UnlockAtRest("123456", DefaultPolicy()); !errors.Is(err, ErrWeakSecret) {
			t.Fatalf("attempt %d: want ErrWeakSecret, got %v", i, err)
		}
	}
	if st := v.LockoutStatus(); st.FailedAttempts != 0 || st.LockedOut {
		t.Fatalf("weak attempts were counted: %+v", st)
	}
	sess, err := v.UnlockAtRest(strongPassphrase, DefaultPolicy())
	if err != nil {
		t.Fatalf("strong passphrase after weak attempts: %v", err)
	}
	sess.Lock()
}

// A vault created with a PIN is upgraded by ChangePassphrase; the DEK is
// re-wrapped, so data sealed before the change still opens.
func TestChangePassphraseUpgradesPINVault(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	sess, err := v.Unlock("654321")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := sess.Seal("ctx", []byte("synthetic"))
	sess.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := v.ChangePassphrase("654321", "123456789", DefaultPolicy(), true); !errors.Is(err, ErrWeakSecret) {
		t.Fatalf("weak new secret: want ErrWeakSecret, got %v", err)
	}
	if err := v.ChangePassphrase("654321", strongPassphrase, DefaultPolicy(), true); err != nil {
		t.Fatalf("ChangePassphrase: %v", err)
	}
	if !v.Status().MeetsThreshold {
		t.Fatal("status must report the new passphrase meets the threshold")
	}
	sess, err = v.UnlockAtRest(strongPassphrase, DefaultPolicy())
	if err != nil {
		t.Fatalf("UnlockAtRest after upgrade: %v", err)
	}
	defer sess.Lock()
	plain, err := sess.Unseal("ctx", blob)
	if err != nil || string(plain) != "synthetic" {
		t.Fatalf("data sealed before the upgrade: %q %v", plain, err)
	}
	if _, err := v.Unlock("654321"); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("the old PIN must no longer unwrap: %v", err)
	}
}

func TestWeakSecretErrorNeverCarriesSecret(t *testing.T) {
	v := newTestVault(t)
	const secret = "s3cr3tz"
	_, err := v.UnlockAtRest(secret, DefaultPolicy())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error must exist and not contain the secret: %v", err)
	}
}

func TestRemoveKeepsLockoutHistory(t *testing.T) {
	v := newTestVault(t)
	if err := v.SetPassphrase(strongPassphrase, DefaultPolicy(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := v.UnlockAtRest(strongPassphrase+"x", DefaultPolicy()); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("want ErrWrongPIN, got %v", err)
	}
	if err := v.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if v.IsConfigured() {
		t.Fatal("vault still configured after Remove")
	}
	if st := v.LockoutStatus(); st.FailedAttempts != 1 {
		t.Fatalf("Remove must keep the failed-attempt history, got %+v", st)
	}
	if err := v.Remove(); err != nil {
		t.Fatalf("Remove of an absent vault: %v", err)
	}
}

// A passphrase shorter than a raised policy minimum is a weak secret on the
// set path too, with the minimum in its message.
func TestSetPassphraseBelowMinLengthIsWeak(t *testing.T) {
	v := newTestVault(t)
	p := DefaultPolicy()
	p.MinLength = 40
	err := v.SetPassphrase(strongPassphrase, p, true)
	if !errors.Is(err, ErrWeakSecret) || !strings.Contains(err.Error(), "40 characters") {
		t.Fatalf("below MinLength: want ErrWeakSecret naming 40 characters, got %v", err)
	}
	if strings.Contains(err.Error(), "master PIN") {
		t.Fatalf("the at-rest message must not talk about a master PIN: %v", err)
	}
}

// TestNonFinitePolicyCannotBypassAtRestFloor: NaN and infinities compare false
// against everything, so a threshold of that kind, whether persisted in
// policy.yaml or passed in directly, must fall back to the 60-bit floor at all
// three at-rest boundaries: weak setup, strong-to-weak rotation, and at-rest
// unlock of a vault whose secret is a short access PIN. The access gate itself
// keeps accepting the short PIN. Mutation check: the previous `t < 60` clamp
// lets NaN through and every subtest fails.
func TestNonFinitePolicyCannotBypassAtRestFloor(t *testing.T) {
	persisted := func(raw string) func(t *testing.T, base string) Policy {
		return func(t *testing.T, base string) Policy {
			t.Helper()
			dir := filepath.Join(base, vaultDirName)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			yml := "min_length: 6\nallow_passphrase: true\ne2e_threshold_bits: " + raw + "\n"
			if err := os.WriteFile(filepath.Join(dir, policyFileName), []byte(yml), 0o600); err != nil {
				t.Fatal(err)
			}
			return LoadPolicy(base)
		}
	}
	direct := func(v float64) func(t *testing.T, base string) Policy {
		return func(*testing.T, string) Policy {
			p := DefaultPolicy()
			p.E2EThresholdBits = v
			return p
		}
	}
	cases := map[string]func(t *testing.T, base string) Policy{
		"persisted NaN":  persisted(".nan"),
		"persisted +Inf": persisted(".inf"),
		"persisted -Inf": persisted("-.inf"),
		"direct NaN":     direct(math.NaN()),
		"direct +Inf":    direct(math.Inf(1)),
		"direct -Inf":    direct(math.Inf(-1)),
	}
	for name, policyFor := range cases {
		t.Run(name, func(t *testing.T) {
			if got := AtRestThresholdBits(policyFor(t, t.TempDir())); got != DefaultE2EThresholdBits {
				t.Fatalf("AtRestThresholdBits = %v, want %v", got, DefaultE2EThresholdBits)
			}

			// Weak setup.
			base := t.TempDir()
			v := Open(base)
			v.paramsOverride = &fastParams
			pol := policyFor(t, base)
			if err := v.SetPassphrase("654321", pol, true); !errors.Is(err, ErrWeakSecret) {
				t.Fatalf("weak setup: want ErrWeakSecret, got %v", err)
			}

			// Strong-to-weak rotation.
			base = t.TempDir()
			v = Open(base)
			v.paramsOverride = &fastParams
			if err := v.SetPassphrase(strongPassphrase, DefaultPolicy(), true); err != nil {
				t.Fatal(err)
			}
			pol = policyFor(t, base)
			if err := v.ChangePassphrase(strongPassphrase, "654321", pol, true); !errors.Is(err, ErrWeakSecret) {
				t.Fatalf("strong-to-weak rotation: want ErrWeakSecret, got %v", err)
			}
			if _, err := v.UnlockAtRest(strongPassphrase, pol); err != nil {
				t.Fatalf("the strong passphrase must still unlock: %v", err)
			}

			// At-rest unlock of an access-PIN vault; the access gate is unchanged.
			base = t.TempDir()
			v = Open(base)
			v.paramsOverride = &fastParams
			pol = policyFor(t, base)
			if err := v.SetPIN("654321", pol, true); err != nil {
				t.Fatalf("access-PIN setup must still work: %v", err)
			}
			if _, err := v.UnlockAtRest("654321", pol); !errors.Is(err, ErrWeakSecret) {
				t.Fatalf("at-rest unlock with a short PIN: want ErrWeakSecret, got %v", err)
			}
			if err := v.VerifyPIN("654321"); err != nil {
				t.Fatalf("VerifyPIN with the access PIN must still work: %v", err)
			}
			sess, err := v.Unlock("654321")
			if err != nil {
				t.Fatalf("Unlock with the access PIN must still work: %v", err)
			}
			sess.Lock()
		})
	}
}

func TestSavePolicyRejectsNonFiniteThreshold(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p := DefaultPolicy()
		p.E2EThresholdBits = v
		if err := SavePolicy(t.TempDir(), p); err == nil {
			t.Fatalf("SavePolicy accepted e2e_threshold_bits=%v", v)
		}
	}
}
