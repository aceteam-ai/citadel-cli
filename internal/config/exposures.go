// internal/config/exposures.go
//
// Durable record of this node's gateway exposures (issue #647).
//
// The gateway's exposure table lives in the running `citadel work` process, so
// before this every exposure vanished on restart — and a worker restart is
// routine: auto-update, WORKER_SELF_HEAL (#548), `systemctl restart`, reboot. A
// link shared from the iOS app or the MCP `expose` verb would therefore start
// 404ing at an arbitrary later time, with no error anywhere and nothing off-node
// aware the route was dropped.
//
// The tell that this was an oversight rather than a decision: the link-token
// SIGNING KEY was already made durable (expose_key.go) so a token minted
// yesterday still verifies today — but the routes those tokens authorize were
// not, so the token outlived the thing it opened.
//
// Records are stored next to that key, 0600, and re-wired at gateway start.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// exposuresFile is the filename (under the config dir) holding the exposure set.
const exposuresFile = "exposures.json"

// ExposureRecord is one persisted exposure: enough to rebuild both the
// reverse-proxy route and its visibility policy verbatim after a restart.
//
// Deliberately NOT stored: the link TOKEN. Tokens are derived on demand from the
// signing key and carry their own expiry, so persisting one would create a
// credential with a longer life than the thing that minted it.
type ExposureRecord struct {
	// Name is the exposed-service slug (the <name> in /expose/<name>/).
	Name string `json:"name"`
	// Port is the loopback host port the route proxies to. Mutually exclusive
	// with Path (issue #943) — exactly one is non-zero/non-empty.
	Port int `json:"port,omitempty"`
	// Path is the resolved, workspace-confined directory served as a static
	// file share instead of a proxy target. Mutually exclusive with Port.
	// Restoring this verbatim on restart is what makes a directory share (like
	// a port-backed one) survive a worker restart (#647).
	Path string `json:"path,omitempty"`
	// Visibility is "private", "org", or "link".
	Visibility string `json:"visibility"`
	// Creator is the tailnet login authorized for a `private` exposure. Empty
	// makes a private exposure inert (fails closed), which is the correct
	// restore behavior too: a private exposure whose creator was never recorded
	// must not come back open.
	Creator string `json:"creator,omitempty"`
	// TokenEpoch is bound into every `link` token this exposure mints. It MUST
	// survive a restart: if it reset to 0, tokens minted under the old epoch
	// would stop verifying (breaking live links), and a later bump could not
	// revoke anything it had already handed out.
	TokenEpoch int `json:"token_epoch,omitempty"`
	// CreatedAt is the RFC3339 timestamp this exposure was FIRST created (issue
	// #944 design doc §3.2). Set on first save; preserved across a later
	// replace-by-name (SaveExposure carries it forward) so the record's
	// identity continuity survives a re-expose. Additive field: a record
	// written before this existed decodes with it empty, and old binaries
	// simply ignore it — no migration.
	CreatedAt string `json:"created_at,omitempty"`
}

// LoadExposures returns the persisted exposure set, or an empty slice when
// nothing has been exposed yet. A missing file is NOT an error (the common case
// on a node that has never exposed anything); a corrupt one IS, so the caller
// can say so loudly rather than silently starting with no routes.
func LoadExposures(configDir string) ([]ExposureRecord, error) {
	data, err := os.ReadFile(filepath.Join(configDir, exposuresFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read exposures: %w", err)
	}
	var recs []ExposureRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse exposures: %w", err)
	}
	return recs, nil
}

// SaveExposure records (or replaces, by name) one exposure. It is called on the
// same path that programs the gateway, so the durable set and the live set are
// written together and cannot diverge.
//
// CreatedAt is carried forward across a replace-by-name (the record's identity
// continuity across re-exposes, design doc §3.2) and set only when the record
// did not previously exist — a caller that leaves it empty on every call (the
// common case; see cmd.liveExposeOps.Expose) still gets correct "first seen"
// semantics without having to look the old record up itself.
func SaveExposure(configDir string, rec ExposureRecord) error {
	if rec.Name == "" {
		return fmt.Errorf("save exposure: empty name")
	}
	recs, err := LoadExposures(configDir)
	if err != nil {
		// A corrupt store must not make the node permanently unable to record new
		// exposures; start a fresh set rather than failing every future expose.
		recs = nil
	}

	replaced := false
	for i := range recs {
		if recs[i].Name == rec.Name {
			if rec.CreatedAt == "" {
				rec.CreatedAt = recs[i].CreatedAt
			}
			recs[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		if rec.CreatedAt == "" {
			rec.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		recs = append(recs, rec)
	}
	return writeExposures(configDir, recs)
}

// FindExposure returns the persisted record for name, or nil if none exists —
// including when the store is corrupt or unreadable. Degrading a corrupt store
// to "not found" (rather than propagating the parse error) mirrors
// SaveExposure's own corrupt-store recovery: a reader that cannot tell "never
// exposed" from "store is corrupt" must still let a new expose proceed rather
// than wedging every future call on this name.
func FindExposure(configDir, name string) *ExposureRecord {
	recs, err := LoadExposures(configDir)
	if err != nil {
		return nil
	}
	for i := range recs {
		if recs[i].Name == name {
			rec := recs[i]
			return &rec
		}
	}
	return nil
}

// DeleteExposure drops one exposure from the durable set. Deleting an absent
// name is a no-op, so an unexpose is idempotent. The returned bool reports
// whether a record actually existed and was removed — a durable record can
// outlive its live gateway route (restored for a port that no longer
// listens, or written by an older build), so a caller cannot infer "was
// anything actually cleaned up here" from the live route alone (citadel
// #967: UNEXPOSE's `removed` signal must account for this).
func DeleteExposure(configDir, name string) (bool, error) {
	recs, err := LoadExposures(configDir)
	if err != nil || len(recs) == 0 {
		return false, nil
	}
	out := recs[:0]
	for _, r := range recs {
		if r.Name != name {
			out = append(out, r)
		}
	}
	if len(out) == len(recs) {
		return false, nil
	}
	if err := writeExposures(configDir, out); err != nil {
		return false, err
	}
	return true, nil
}

// writeExposures persists recs 0600, sorted by name for a stable file, via a
// temp file + rename so a crash mid-write cannot leave a truncated store that
// would drop every exposure on the next start.
func writeExposures(configDir string, recs []ExposureRecord) error {
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })

	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("encode exposures: %w", err)
	}
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	path := filepath.Join(configDir, exposuresFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write exposures: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("persist exposures: %w", err)
	}
	return nil
}
