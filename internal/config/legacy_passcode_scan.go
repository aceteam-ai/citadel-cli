// internal/config/legacy_passcode_scan.go
//
// Support for the master-PIN enroll ownership check (citadel#808 fast-follow).
// See cmd/passcode_master.go for the caller and the full rationale.
package config

import (
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// LegacyPasscodeSearchResult is what FindLegacyPasscode found when it looked
// for an existing legacy bcrypt passcode hash across a set of candidate
// ConfigDirs.
type LegacyPasscodeSearchResult struct {
	// Dirs is the config directory of every candidate where a non-empty
	// PasscodeHash was found (index-aligned with Hashes). Usually zero or one
	// entry; more than one means stale hashes were left behind under more
	// than one invocation context.
	Dirs []string
	// Hashes are the bcrypt hashes found, index-aligned with Dirs.
	Hashes []string
	// Inconclusive is true when at least one candidate directory could not be
	// read for a reason OTHER than "does not exist" (permission denied, a
	// malformed file, or another I/O error) — meaning an empty Hashes is NOT
	// reliable proof that no legacy passcode was ever set anywhere on this
	// machine. Callers MUST treat Inconclusive as "cannot rule out a legacy
	// passcode" and fail closed rather than treating it the same as a clean
	// "found nothing".
	Inconclusive bool
}

// Found reports whether at least one legacy hash was located.
func (r LegacyPasscodeSearchResult) Found() bool { return len(r.Hashes) > 0 }

// Verify reports whether pin matches EVERY found hash (logical AND, not OR),
// and requires at least one hash to exist. An empty pin never matches
// (mirrors Permissions.VerifyPasscode's fail-closed handling of an empty
// secret).
//
// AND, not OR, is deliberate: candidates are scanned across every plausible
// ConfigDir on the machine (see FindLegacyPasscode), some of which an
// unprivileged invoker may be able to write to (e.g. their own
// ~/.citadel-cli). If Verify accepted a match against ANY found hash, that
// invoker could plant a hash of a PIN they already know and "prove
// ownership" with it, regardless of what the real legacy passcode actually
// is. Requiring a match against every found hash costs nothing in the normal
// single-hash case (there is nothing else to AND against) and means a
// self-planted hash can only ever ADD a requirement, never substitute for
// the real one.
func (r LegacyPasscodeSearchResult) Verify(pin string) bool {
	if pin == "" || len(r.Hashes) == 0 {
		return false
	}
	for _, h := range r.Hashes {
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(pin)) != nil {
			return false
		}
	}
	return true
}

// FindLegacyPasscode scans every directory in dirs for a legacy bcrypt
// passcode hash (permissions.yaml's passcode_hash field). It reads the raw
// file directly rather than going through LoadPermissions, because
// LoadPermissions' default-filling would make an absent file
// indistinguishable from an intentional "no passcode" — here, a candidate
// directory with no file at all must mean exactly "no hash there", nothing
// more.
//
// This exists because platform.ConfigDir() is invoker-scoped (see its doc
// comment): the directory a given 'citadel passcode enroll' invocation
// resolves to may not be the directory an earlier 'citadel passcode set' (or
// an APPLY_DEVICE_CONFIG push handled by a differently-privileged process)
// wrote to. Checking only the current invocation's ConfigDir() could let
// enroll conclude "no legacy passcode" when one genuinely exists elsewhere on
// the same machine, silently skipping the ownership proof it exists to
// enforce. Callers should pass platform.ConfigDirCandidates().
func FindLegacyPasscode(dirs []string) LegacyPasscodeSearchResult {
	var result LegacyPasscodeSearchResult
	for _, dir := range dirs {
		path := filepath.Join(dir, permissionsFile)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // this candidate simply doesn't have one; not ambiguous
			}
			// Exists (or existence couldn't be determined) but unreadable: we
			// cannot rule out a hash living here.
			result.Inconclusive = true
			continue
		}
		var p Permissions
		if err := yaml.Unmarshal(data, &p); err != nil {
			result.Inconclusive = true
			continue
		}
		if p.PasscodeHash != "" {
			result.Dirs = append(result.Dirs, dir)
			result.Hashes = append(result.Hashes, p.PasscodeHash)
		}
	}
	return result
}
