package nodeidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
)

// TestFingerprintPublicKey_MatchesStorePublicKeyFingerprint pins that the free
// function and the Store method compute the IDENTICAL fingerprint for the same
// key — the whole point of extracting FingerprintPublicKey is that the signer
// (via the method) and a verifier (via the free function) can never drift.
func TestFingerprintPublicKey_MatchesStorePublicKeyFingerprint(t *testing.T) {
	store := New(t.TempDir())
	key, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	fromMethod, err := store.PublicKeyFingerprint()
	if err != nil {
		t.Fatalf("PublicKeyFingerprint: %v", err)
	}
	fromFunc, err := FingerprintPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("FingerprintPublicKey: %v", err)
	}
	if fromMethod != fromFunc {
		t.Errorf("method fingerprint %q != free-function fingerprint %q", fromMethod, fromFunc)
	}
}

func TestFingerprintPublicKey_NilKey(t *testing.T) {
	if _, err := FingerprintPublicKey(nil); err == nil {
		t.Fatal("FingerprintPublicKey(nil) returned nil error, want error")
	}
}

// TestPublicKey_ReturnsPersistedKeyWithoutGenerating pins that PublicKey loads
// an existing key and returns its public half.
func TestPublicKey_ReturnsPersistedKeyWithoutGenerating(t *testing.T) {
	store := New(t.TempDir())
	priv, err := store.GetOrCreateKey()
	if err != nil {
		t.Fatalf("GetOrCreateKey: %v", err)
	}
	pub, err := store.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if !pub.Equal(&priv.PublicKey) {
		t.Error("PublicKey returned a key that does not match the persisted private key")
	}
}

// TestPublicKey_NeverGeneratesWhenAbsent is the load-bearing contract: unlike
// Sign/PublicKeyFingerprint, PublicKey must NOT mint a key as a side effect —
// a verifier that silently created identity would mask the honest "no key
// present" failure and leave a leafless key behind.
func TestPublicKey_NeverGeneratesWhenAbsent(t *testing.T) {
	store := New(t.TempDir())
	if store.HasKey() {
		t.Fatal("fresh store already has a key")
	}
	if _, err := store.PublicKey(); err == nil {
		t.Fatal("PublicKey on an empty store returned nil error, want a missing-key error")
	}
	if store.HasKey() {
		t.Fatal("PublicKey generated a key as a side effect; it must be read-only")
	}
}

// sanity: keep the imports honest and confirm P-256 keys round-trip through the
// fingerprint helper deterministically.
func TestFingerprintPublicKey_Deterministic(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	a, _ := FingerprintPublicKey(&key.PublicKey)
	b, _ := FingerprintPublicKey(&key.PublicKey)
	if a == "" || a != b {
		t.Errorf("non-deterministic fingerprint: %q vs %q", a, b)
	}
}
