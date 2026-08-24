package nodevault

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/hkdf"
)

// ErrLocked is returned by a Session whose DEK has been discarded.
var ErrLocked = errors.New("nodevault: session is locked")

// sessionCiphertextVersion prefixes every Seal output so the format can evolve.
const sessionCiphertextVersion = 1

// Session is a short-lived unlocked handle. The master DEK lives only in the
// memory backing a Session; it is never written to disk and never exposed to
// callers. Consumers obtain context-scoped subkeys (DeriveSubkey) or use
// Seal/Unseal, which derive a per-context subkey internally — the raw DEK never
// leaves this package.
type Session struct {
	mu  sync.Mutex
	dek []byte // nil once locked
}

func newSession(dek []byte) *Session {
	// Copy so the caller's deferred zero() of the source buffer cannot wipe the
	// session's DEK out from under it.
	cp := make([]byte, len(dek))
	copy(cp, dek)
	return &Session{dek: cp}
}

// IsUnlocked reports whether the DEK is still resident.
func (s *Session) IsUnlocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dek != nil
}

// Lock discards the DEK. Idempotent. After Lock, Seal/Unseal/DeriveSubkey
// return ErrLocked. There is no on-disk cached key, so a locked session cannot
// be revived without the PIN — this is what keeps encrypted surfaces dark
// across an unattended restart (docs §2, §4).
func (s *Session) Lock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dek != nil {
		zero(s.dek)
		s.dek = nil
	}
}

// DeriveSubkey returns a 32-byte HKDF-SHA256 subkey bound to context. Different
// contexts yield independent, non-invertible keys; none of them reveal the DEK.
// A consumer that manages its own encryption (e.g. #795's browser profile)
// takes a subkey for its own scope rather than the master key.
func (s *Session) DeriveSubkey(context string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dek == nil {
		return nil, ErrLocked
	}
	return deriveSubkey(s.dek, context)
}

// Seal encrypts plaintext under a subkey derived from context. The returned
// ciphertext is self-describing: [version][nonce][AES-256-GCM ciphertext+tag].
// context is bound as GCM additional data, so a ciphertext produced under one
// context cannot be Unsealed under another even by the same session.
func (s *Session) Seal(context string, plaintext []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dek == nil {
		return nil, ErrLocked
	}
	subkey, err := deriveSubkey(s.dek, context)
	if err != nil {
		return nil, err
	}
	defer zero(subkey)

	gcm, err := newGCM(subkey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nodevault: seal nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+gcm.Overhead())
	out = append(out, sessionCiphertextVersion)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, []byte(context))
	return out, nil
}

// Unseal reverses Seal. It errors (never returns partial plaintext) on any
// integrity failure, including a context mismatch.
func (s *Session) Unseal(context string, ciphertext []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dek == nil {
		return nil, ErrLocked
	}
	subkey, err := deriveSubkey(s.dek, context)
	if err != nil {
		return nil, err
	}
	defer zero(subkey)

	gcm, err := newGCM(subkey)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < 1+gcm.NonceSize() {
		return nil, errors.New("nodevault: ciphertext too short")
	}
	if ciphertext[0] != sessionCiphertextVersion {
		return nil, fmt.Errorf("nodevault: unknown ciphertext version %d", ciphertext[0])
	}
	nonce := ciphertext[1 : 1+gcm.NonceSize()]
	body := ciphertext[1+gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, body, []byte(context))
	if err != nil {
		return nil, fmt.Errorf("nodevault: unseal: %w", err)
	}
	return pt, nil
}

// deriveSubkey = HKDF-SHA256(dek, info="nodevault/context/"+context).
func deriveSubkey(dek []byte, context string) ([]byte, error) {
	r := hkdf.New(sha256.New, dek, nil, []byte("nodevault/context/"+context))
	sk := make([]byte, 32)
	if _, err := io.ReadFull(r, sk); err != nil {
		return nil, fmt.Errorf("nodevault: derive subkey: %w", err)
	}
	return sk, nil
}
