package nodevault

// Enroll performs the one-time migration from the legacy bcrypt passcode gate
// to the master-PIN vault (issue #796). A bcrypt hash cannot yield a key, so
// there is no silent migration: the user re-enters their PIN, the vault is set
// up under it (or a new, stronger policy-compliant secret), and the legacy
// hash is deleted so it stops being a cheap offline brute-force target that
// undoes the KDF.
//
// Dependency inversion keeps this package pure: the legacy hash lives in
// internal/config (bcrypt), which this package must not import. The caller
// passes:
//
//   - legacyVerify: proves ownership by checking a re-entered legacy PIN
//     against the existing bcrypt hash. Pass nil when there is no legacy
//     passcode set (a fresh node) — then this is just an initial SetPIN and no
//     ownership proof is needed.
//   - deleteLegacy: deletes the bcrypt PasscodeHash and persists. It MUST be
//     idempotent (clearing an already-empty hash is a harmless no-op), because
//     Enroll may re-run it to finish a migration whose delete step failed.
//
// legacyPIN proves the old secret (which may be a 4-digit legacy PIN); newPIN
// is the master secret being set and is validated against policy (default
// 6-char minimum). They may differ, so a node can migrate a 4-digit legacy PIN
// to a policy-compliant master PIN in a single flow.
//
// Ordering and failure modes:
//   - The vault is written durably (atomic temp-file + fsync + rename) BEFORE
//     the legacy hash is deleted. If the process dies between the two, the
//     vault exists and the legacy hash lingers — never the reverse (which
//     would brick the node).
//   - If deleteLegacy fails, Enroll returns the error but the vault is already
//     set. Re-running Enroll is idempotent: it sees the vault configured, skips
//     re-setup, and just retries deleteLegacy to clean up the lingering hash.
func (v *Vault) Enroll(
	legacyPIN, newPIN string,
	policy Policy,
	ackDataLoss bool,
	legacyVerify func(pin string) bool,
	deleteLegacy func() error,
) error {
	if !ackDataLoss {
		return ErrAckRequired
	}

	// Idempotent recovery: a prior Enroll set the vault but failed to delete the
	// legacy hash. Don't re-verify or re-set; just finish the deletion.
	if v.IsConfigured() {
		if deleteLegacy != nil {
			return deleteLegacy()
		}
		return nil
	}

	if err := policy.Validate(newPIN); err != nil {
		return err
	}
	if legacyVerify != nil && !legacyVerify(legacyPIN) {
		return ErrWrongPIN
	}

	if err := v.SetPIN(newPIN, policy, ackDataLoss); err != nil {
		return err
	}
	if deleteLegacy != nil {
		return deleteLegacy()
	}
	return nil
}
