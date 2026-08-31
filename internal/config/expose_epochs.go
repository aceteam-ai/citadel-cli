// internal/config/expose_epochs.go
//
// Epoch high-water store for gateway exposures (issue #944, design doc §5.3).
//
// This is the piece of durable memory that makes the epoch-regression guard
// actually closed rather than just "closed until the record is deleted":
// exposures.json's TokenEpoch is authoritative ONLY while a record exists, but
// DeleteExposure (UNEXPOSE) removes it entirely. Without a second, independent
// memory of "the highest epoch this name has EVER lived at", a later re-expose
// of the same name has nothing to compare against and can silently resurrect
// tokens that were already revoked (or that were valid immediately before an
// explicit teardown) — the exact landmine issue #944 exists to close.
//
// Entries here are NEVER deleted — that is the point: it is the memory that
// survives DeleteExposure. Growth is bounded by the number of distinct
// operator-chosen slugs a node has ever exposed, which is negligible.
//
// Lenient-read posture matches exposures.json's own: a missing/corrupt file
// degrades to "no recorded high-water" (0) rather than failing every future
// expose of every name. For a name with a LIVE record, base epoch resolution
// (cmd.liveExposeOps.Expose) reads TokenEpoch from that record first, so the
// common case loses nothing when this file is lost; the loss is scoped to the
// post-unexpose memory this file exists to add. Mirrors the reasoning already
// applied to SaveExposure's own corrupt-store recovery (exposures.go) and to
// TokenHashEntry.UnmarshalJSON's lenient parse (citadel #815, see CLAUDE.md).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// exposeEpochsFile is the filename (under the config dir, alongside
// exposures.json) holding the per-name epoch high-water map.
const exposeEpochsFile = "expose_epochs.json"

// loadExposeEpochHighWater returns the persisted {name: highest-epoch-ever}
// map, degrading a missing or corrupt file to an empty map.
func loadExposeEpochHighWater(configDir string) map[string]int {
	data, err := os.ReadFile(filepath.Join(configDir, exposeEpochsFile))
	if err != nil {
		return map[string]int{}
	}
	var m map[string]int
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]int{}
	}
	if m == nil {
		m = map[string]int{}
	}
	return m
}

// ExposeEpochHighWater returns the highest epoch ever recorded for name, or 0
// if the name has never been recorded (never exposed, or the store is
// missing/corrupt).
func ExposeEpochHighWater(configDir, name string) int {
	return loadExposeEpochHighWater(configDir)[name]
}

// SaveExposeEpochHighWater durably records epoch as name's new high-water
// mark. The mark can never regress: if epoch is not strictly greater than the
// currently recorded value, this is a no-op (both semantically — the
// invariant this store exists to protect — and as a write optimization, since
// the common "plain re-expose preserves the epoch" path would otherwise
// rewrite this file on every single expose call).
func SaveExposeEpochHighWater(configDir, name string, epoch int) error {
	m := loadExposeEpochHighWater(configDir)
	if epoch <= m[name] {
		return nil
	}
	m[name] = epoch

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	path := filepath.Join(configDir, exposeEpochsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
