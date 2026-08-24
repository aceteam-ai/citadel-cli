package nodevault

import (
	"fmt"
	"unicode"
)

// Policy is the node-level master-PIN policy. It is a CONFIG VALUE from day one
// (issue #796 amendment): a node sets a minimum length and whether a
// non-numeric passphrase is allowed, so a user may choose anything from a
// 6-digit PIN up to a full passphrase. Nothing here is a hardcoded constant at
// the setter boundary.
type Policy struct {
	// MinLength is the minimum number of characters. Default 6.
	MinLength int `yaml:"min_length"`
	// AllowPassphrase permits non-numeric secrets. When false, the secret must
	// be all digits (a classic PIN). Default true.
	AllowPassphrase bool `yaml:"allow_passphrase"`
	// E2EThresholdBits is the estimated-entropy bar above which the unqualified
	// "end-to-end encrypted" badge is truthful. See entropy.go for the number
	// and the reasoning. Default DefaultE2EThresholdBits.
	E2EThresholdBits float64 `yaml:"e2e_threshold_bits"`
}

// DefaultMinPINLength is the default minimum master-PIN length: 6 digits
// (~20 bits), matching common device-passcode norms (issue #796 amendment).
const DefaultMinPINLength = 6

// DefaultPolicy returns the shipping default: 6-character minimum, passphrases
// allowed, badge threshold at DefaultE2EThresholdBits.
func DefaultPolicy() Policy {
	return Policy{
		MinLength:        DefaultMinPINLength,
		AllowPassphrase:  true,
		E2EThresholdBits: DefaultE2EThresholdBits,
	}
}

// normalized fills zero-valued fields with defaults so a caller that passes a
// partially-populated Policy (or the zero value) still gets sane enforcement
// rather than MinLength==0.
func (p Policy) normalized() Policy {
	if p.MinLength <= 0 {
		p.MinLength = DefaultMinPINLength
	}
	if p.E2EThresholdBits <= 0 {
		p.E2EThresholdBits = DefaultE2EThresholdBits
	}
	return p
}

// Validate checks a candidate secret against the policy. It runs before the
// KDF at every set/rotate boundary.
func (p Policy) Validate(secret string) error {
	pol := p.normalized()
	if len([]rune(secret)) < pol.MinLength {
		return fmt.Errorf("nodevault: master PIN must be at least %d characters", pol.MinLength)
	}
	if !pol.AllowPassphrase && !allDigits(secret) {
		return fmt.Errorf("nodevault: master PIN must be numeric (passphrases are disabled by node policy)")
	}
	return nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
