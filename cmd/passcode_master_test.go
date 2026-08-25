package cmd

import (
	"errors"
	"testing"
)

func TestEnrollPrecheck(t *testing.T) {
	cases := []struct {
		name          string
		vaultConfig   bool
		hasLegacyHash bool
		wantCleanup   bool
		wantErr       error
	}{
		{"fresh node", false, false, false, nil},
		{"fresh node with legacy passcode", false, true, false, nil},
		{"already enrolled, clean", true, false, false, errAlreadyEnrolled},
		{"interrupted migration (vault set, legacy lingers)", true, true, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cleanup, err := enrollPrecheck(c.vaultConfig, c.hasLegacyHash)
			if cleanup != c.wantCleanup {
				t.Errorf("cleanup=%v want %v", cleanup, c.wantCleanup)
			}
			if !errors.Is(err, c.wantErr) {
				t.Errorf("err=%v want %v", err, c.wantErr)
			}
		})
	}
}
