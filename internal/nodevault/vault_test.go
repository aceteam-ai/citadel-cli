package nodevault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fastParams keeps the KDF cheap so the suite is not tens of seconds of
// Argon2id. TestRealDefaultParams below exercises the shipping cost once.
var fastParams = argonParams{Time: 1, Memory: 64, Threads: 1, KeyLen: 32}

func newTestVault(t *testing.T) *Vault {
	t.Helper()
	v := Open(t.TempDir())
	v.paramsOverride = &fastParams
	return v
}

func mustSet(t *testing.T, v *Vault, pin string) {
	t.Helper()
	if err := v.SetPIN(pin, DefaultPolicy(), true); err != nil {
		t.Fatalf("SetPIN(%q): %v", pin, err)
	}
}

func TestSetAndVerify(t *testing.T) {
	v := newTestVault(t)
	if v.IsConfigured() {
		t.Fatal("fresh vault reports configured")
	}
	mustSet(t, v, "654321")
	if !v.IsConfigured() {
		t.Fatal("vault not configured after SetPIN")
	}
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("VerifyPIN correct: %v", err)
	}
	if err := v.VerifyPIN("000000"); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("VerifyPIN wrong: want ErrWrongPIN, got %v", err)
	}
}

func TestSetRequiresAck(t *testing.T) {
	v := newTestVault(t)
	if err := v.SetPIN("654321", DefaultPolicy(), false); !errors.Is(err, ErrAckRequired) {
		t.Fatalf("want ErrAckRequired, got %v", err)
	}
	if v.IsConfigured() {
		t.Fatal("vault configured despite missing acknowledgement")
	}
}

func TestSetTwiceRejected(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	if err := v.SetPIN("111111", DefaultPolicy(), true); !errors.Is(err, ErrAlreadyConfigured) {
		t.Fatalf("want ErrAlreadyConfigured, got %v", err)
	}
	// The original PIN must still work (a rejected re-set never clobbers).
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("original PIN broken after rejected re-set: %v", err)
	}
}

func TestPolicyConfigurableLength(t *testing.T) {
	v := newTestVault(t)
	// Default policy: 6-char minimum.
	if err := v.SetPIN("123", DefaultPolicy(), true); err == nil {
		t.Fatal("3-char PIN accepted under default 6-char policy")
	}
	// A stricter node policy raises the minimum.
	strict := Policy{MinLength: 10, AllowPassphrase: true}
	if err := v.SetPIN("123456", strict, true); err == nil {
		t.Fatal("6-char PIN accepted under 10-char policy")
	}
	if err := v.SetPIN("correct horse", strict, true); err != nil {
		t.Fatalf("passphrase rejected under passphrase-allowing policy: %v", err)
	}
}

func TestPolicyNumericOnly(t *testing.T) {
	v := newTestVault(t)
	numeric := Policy{MinLength: 6, AllowPassphrase: false}
	if err := v.SetPIN("letters", numeric, true); err == nil {
		t.Fatal("non-numeric secret accepted when passphrases disabled")
	}
	if err := v.SetPIN("654321", numeric, true); err != nil {
		t.Fatalf("numeric PIN rejected under numeric policy: %v", err)
	}
}

func TestRotationRewrapsSameDEK(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")

	sess, err := v.Unlock("654321")
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	before, err := sess.DeriveSubkey("browser-profile")
	if err != nil {
		t.Fatalf("derive before: %v", err)
	}
	sess.Lock()

	if err := v.ChangePIN("654321", "abcdef123456", DefaultPolicy(), true); err != nil {
		t.Fatalf("ChangePIN: %v", err)
	}

	// Old PIN must now fail; new PIN must unlock.
	if err := v.VerifyPIN("654321"); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("old PIN still works after rotation: %v", err)
	}
	sess2, err := v.Unlock("abcdef123456")
	if err != nil {
		t.Fatalf("unlock with new PIN: %v", err)
	}
	after, err := sess2.DeriveSubkey("browser-profile")
	if err != nil {
		t.Fatalf("derive after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("subkey changed across rotation: DEK was NOT preserved (data would be unreadable)")
	}
}

func TestRotationWrongOldPIN(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	if err := v.ChangePIN("999999", "newsecret1", DefaultPolicy(), true); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("want ErrWrongPIN, got %v", err)
	}
	// The PIN must be unchanged.
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("PIN changed despite failed rotation: %v", err)
	}
}

func TestSealUnsealRoundTrip(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	sess, err := v.Unlock("654321")
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	defer sess.Lock()

	plain := []byte("browser cookie jar")
	ct, err := sess.Seal("ctx-a", plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := sess.Unseal("ctx-a", ct)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestSealContextBinding(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	sess, _ := v.Unlock("654321")
	defer sess.Lock()

	ct, err := sess.Seal("ctx-a", []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := sess.Unseal("ctx-b", ct); err == nil {
		t.Fatal("ciphertext unsealed under the WRONG context")
	}
}

func TestSubkeysDifferByContextAndNeverEqualDEK(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	sess, _ := v.Unlock("654321")
	defer sess.Lock()

	a, _ := sess.DeriveSubkey("a")
	b, _ := sess.DeriveSubkey("b")
	if bytes.Equal(a, b) {
		t.Fatal("different contexts produced identical subkeys")
	}
	// The raw DEK is never exposed by the API; the subkey must not equal the
	// session's internal DEK.
	if bytes.Equal(a, sess.dek) {
		t.Fatal("subkey equals the master DEK")
	}
}

func TestLockedSessionRefuses(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	sess, _ := v.Unlock("654321")
	sess.Lock()
	if sess.IsUnlocked() {
		t.Fatal("session reports unlocked after Lock")
	}
	if _, err := sess.Seal("c", []byte("x")); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked from Seal, got %v", err)
	}
	if _, err := sess.DeriveSubkey("c"); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked from DeriveSubkey, got %v", err)
	}
}

func TestTamperDetected(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")

	// Flip a byte in the wrapped DEK; the AEAD tag must reject it as a wrong
	// PIN rather than returning garbage key material.
	h, err := v.readHeader()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := unb64(h.Wraps[0].WrappedDEK)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xFF // flip a tag byte -> valid base64, invalid AEAD
	h.Wraps[0].WrappedDEK = b64(raw)
	if err := writeHeaderAtomic(v.headerPath(), h); err != nil {
		t.Fatal(err)
	}

	if err := v.VerifyPIN("654321"); err == nil {
		t.Fatal("tampered vault verified successfully")
	}
}

func TestPepperRequiredForUnlock(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	// Losing the pepper file makes the vault unrecoverable even with the right
	// PIN — the pepper is a required KDF input (data-loss failure mode).
	if err := os.Remove(v.pepperPath()); err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyPIN("654321"); err == nil {
		t.Fatal("verified without the pepper present")
	}
}

func TestEmptyPINDoesNotConsumeLockout(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	// An empty PIN is always wrong but must never move the lockout counter,
	// or unauthenticated per-connection probes could self-DoS an enrolled node.
	for i := 0; i < maxFreeAttempts*3; i++ {
		if err := v.VerifyPIN(""); !errors.Is(err, ErrWrongPIN) {
			t.Fatalf("empty PIN attempt %d: want ErrWrongPIN, got %v", i, err)
		}
	}
	if lo := v.LockoutStatus(); lo.FailedAttempts != 0 || lo.LockedOut {
		t.Fatalf("empty PIN probes moved the lockout counter: %+v", lo)
	}
	// The correct PIN still works (never locked).
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("correct PIN after empty probes: %v", err)
	}
}

func TestEntropyGatedBadge(t *testing.T) {
	// 6-digit PIN -> caveated (below threshold).
	v1 := newTestVault(t)
	mustSet(t, v1, "654321")
	if st := v1.Status(); st.MeetsThreshold {
		t.Fatalf("6-digit PIN incorrectly meets E2E threshold (%.1f bits)", st.EntropyBits)
	}
	// A long passphrase -> unqualified badge (above threshold).
	v2 := newTestVault(t)
	mustSet(t, v2, "correct-horse-battery-staple-9")
	if st := v2.Status(); !st.MeetsThreshold {
		t.Fatalf("strong passphrase does not meet E2E threshold (%.1f bits)", st.EntropyBits)
	}
}

func TestOnDiskNoPlaintextSecret(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	data, err := os.ReadFile(v.headerPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "654321") {
		t.Fatal("plaintext PIN present in vault file")
	}
}

func TestFilePermissions(t *testing.T) {
	v := newTestVault(t)
	mustSet(t, v, "654321")
	for _, p := range []string{v.headerPath(), v.pepperPath()} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", p, fi.Mode().Perm())
		}
	}
	di, err := os.Stat(filepath.Dir(v.headerPath()))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("vault dir mode = %o, want 0700", di.Mode().Perm())
	}
}

// TestRealDefaultParams exercises the shipping Argon2id cost once, so the
// default path is not left entirely untested by the fast-param override.
func TestRealDefaultParams(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real-KDF test in -short mode")
	}
	v := Open(t.TempDir()) // no override: 64 MiB, time=3
	if p := v.newParams(); p != defaultArgonParams() {
		t.Fatalf("default vault params = %+v, want %+v", p, defaultArgonParams())
	}
	if err := v.SetPIN("654321", DefaultPolicy(), true); err != nil {
		t.Fatalf("SetPIN with real params: %v", err)
	}
	if err := v.VerifyPIN("654321"); err != nil {
		t.Fatalf("VerifyPIN with real params: %v", err)
	}
	if err := v.VerifyPIN("000000"); !errors.Is(err, ErrWrongPIN) {
		t.Fatalf("wrong PIN under real params: %v", err)
	}
}
