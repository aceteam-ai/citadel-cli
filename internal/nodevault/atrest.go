package nodevault

import (
	"errors"
	"fmt"
	"os"
)

// At-rest consumers (#820). The vault's single secret is both the online
// access gate and the key root, and the access gate is allowed to be a short
// PIN. A short PIN cannot protect data at rest: whoever has the disk also has
// the salt, the KDF parameters and the pepper, so the only unknown is the
// secret itself, and 10^6 guesses are cheap even under Argon2id. So a surface
// that ENCRYPTS DATA AT REST must use the AtRest entry points below, which
// refuse a secret whose estimated entropy is below AtRestThresholdBits. The
// access-gate entry points (SetPIN, Unlock, VerifyPIN) are unchanged.
//
// The check runs on the secret the caller supplies, never on the header's
// meets_e2e_threshold bit, which is unauthenticated and could be edited to
// read "strong". A strong secret that unwraps the DEK proves the only wrap is
// strong, because the vault has exactly one wrap.

// ErrWeakSecret is returned when a secret is too weak to encrypt data at rest.
// It never carries the secret or its estimated strength.
var ErrWeakSecret = errors.New("nodevault: this secret is too short to protect data stored on disk; use a passphrase of four or more random words")

// AtRestThresholdBits is the minimum estimated entropy for an at-rest secret:
// the policy's E2E threshold, but never below DefaultE2EThresholdBits, so a
// policy file cannot lower the bar for data at rest.
func AtRestThresholdBits(p Policy) float64 {
	t := p.normalized().E2EThresholdBits
	if t < DefaultE2EThresholdBits {
		t = DefaultE2EThresholdBits
	}
	return t
}

// ValidateAtRest checks secret against the at-rest threshold and the policy.
// A secret that is too weak or shorter than the policy minimum is
// ErrWeakSecret (so callers show one message); a secret the policy rejects for
// its shape (a PIN-only policy) keeps the policy's own error.
func ValidateAtRest(secret string, p Policy) error {
	if estimateEntropyBits(secret) < AtRestThresholdBits(p) {
		return ErrWeakSecret
	}
	if min := p.normalized().MinLength; len([]rune(secret)) < min {
		return fmt.Errorf("%w (use at least %d characters)", ErrWeakSecret, min)
	}
	return p.Validate(secret)
}

// MeetsAtRest reports whether secret is strong enough to encrypt data at rest,
// without saying why not. Callers use it for an early, friendly message.
func MeetsAtRest(secret string, p Policy) bool {
	return ValidateAtRest(secret, p) == nil
}

// SetPassphrase creates the vault with an at-rest-grade passphrase. It is
// SetPIN plus the #820 threshold.
func (v *Vault) SetPassphrase(passphrase string, policy Policy, ackDataLoss bool) error {
	if !ackDataLoss {
		return ErrAckRequired
	}
	if err := ValidateAtRest(passphrase, policy); err != nil {
		return err
	}
	return v.SetPIN(passphrase, policy, ackDataLoss)
}

// ChangePassphrase rotates the vault secret to an at-rest-grade passphrase.
// The current secret only authorizes the change, so it may be a short PIN:
// this is how a vault created with a PIN is upgraded. The DEK is re-wrapped,
// never replaced.
func (v *Vault) ChangePassphrase(current, passphrase string, policy Policy, ackDataLoss bool) error {
	if !ackDataLoss {
		return ErrAckRequired
	}
	if err := ValidateAtRest(passphrase, policy); err != nil {
		return err
	}
	return v.ChangePIN(current, passphrase, policy, ackDataLoss)
}

// UnlockAtRest is Unlock for a consumer that encrypts data at rest. A secret
// below the at-rest threshold is refused before the KDF runs and is not
// counted toward lockout: it cannot be the secret of a vault usable at rest,
// so the refusal reveals nothing about the vault, and an unauthenticated
// caller cannot lock the vault out with free, short guesses. Every other
// outcome (wrong secret, lockout, unconfigured) is Unlock's.
func (v *Vault) UnlockAtRest(secret string, policy Policy) (*Session, error) {
	if secret == "" {
		return nil, ErrWrongPIN
	}
	if err := ValidateAtRest(secret, policy); err != nil {
		if errors.Is(err, ErrWeakSecret) {
			return nil, err
		}
		// A policy shape refusal (non-numeric under a PIN-only policy) also
		// means this secret cannot be the vault's at-rest secret.
		return nil, fmt.Errorf("%w (%v)", ErrWeakSecret, err)
	}
	return v.Unlock(secret)
}

// Remove deletes the vault's header and pepper, so it reads as not configured
// and nothing sealed under it can ever be opened again. It is the escape hatch
// for a forgotten secret under the no-recovery model, for a vault whose caller
// has already deleted (or is about to abandon) the data it protected. The
// lockout state is kept, so removing a vault never clears failed attempts.
func (v *Vault) Remove() error {
	v.kdfMu.Lock()
	defer v.kdfMu.Unlock()
	if err := os.Remove(v.headerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("nodevault: remove header: %w", err)
	}
	if err := os.Remove(v.pepperPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("nodevault: remove pepper: %w", err)
	}
	return nil
}

// DirName is the subdirectory of a vault's base dir that holds the vault.
const DirName = vaultDirName
