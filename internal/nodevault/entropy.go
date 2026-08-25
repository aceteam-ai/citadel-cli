package nodevault

import (
	"math"
	"unicode"
)

// DefaultE2EThresholdBits is the estimated-entropy bar for the UNQUALIFIED
// "end-to-end encrypted" badge.
//
// Why 60 bits: the badge must not overclaim against the threat this whole
// design targets — offline brute force of a stolen disk, where the attacker
// has the salt, the Argon2id params, AND the pepper file (it sits on the same
// disk). Argon2id raises the cost per guess but not the guess space, so the
// only real defense left is the secret's own entropy. A 6-digit PIN is ~20
// bits (10^6 ≈ 2^20) — trivially exhaustible even at a high KDF cost, so it
// gets a CAVEATED indicator, never the strong claim. ~60 bits (roughly a
// 12–13 char mixed passphrase, or a 5-word passphrase) is the point where the
// combined KDF-cost × guess-space search is infeasible for a realistic
// attacker, so at or above it the unqualified badge is truthful. The exact
// number is a policy knob (Policy.E2EThresholdBits), not a security proof.
const DefaultE2EThresholdBits = 60.0

// estimateEntropyBits returns a deliberately CONSERVATIVE estimate of a
// secret's entropy in bits: len × log2(character-pool). It assumes the pool is
// the union of the character classes actually present, which OVER-estimates a
// human-chosen passphrase (real language has far less than log2(pool) bits per
// character). It is a floor for showing a badge, never a strength guarantee —
// which is exactly why the badge below the threshold is caveated, not trusted.
func estimateEntropyBits(secret string) float64 {
	if secret == "" {
		return 0
	}
	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, r := range secret {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasSymbol = true
		}
	}
	pool := 0
	if hasLower {
		pool += 26
	}
	if hasUpper {
		pool += 26
	}
	if hasDigit {
		pool += 10
	}
	if hasSymbol {
		pool += 32 // rough printable-symbol count
	}
	if pool == 0 {
		return 0
	}
	return float64(len([]rune(secret))) * math.Log2(float64(pool))
}
