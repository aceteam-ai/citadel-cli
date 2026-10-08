package network

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// identityRecordFile is this node's durable record of its last confirmed
// Headscale identity, written to <nodeConfigDir>/node-identity.json -- a
// SIBLING of the network/ state dir, so ClearState() (which removes only
// network/) never deletes it. It lets recovery report an identity churn
// honestly ("was X") instead of a bare "IP may have changed", and keeps a
// stable record of the node's last good registration for diagnostics (#1235).
// Distinct from cmd/whoami's identity.json (a display cache) and the ECDSA
// identity/ dir.
const identityRecordFile = "node-identity.json"

// IdentityRecord is the persisted snapshot of the node's last confirmed mesh
// identity. All fields are best-effort; an absent field is "".
type IdentityRecord struct {
	HeadscaleNodeID string `json:"headscale_node_id,omitempty"`
	MeshIP          string `json:"mesh_ip,omitempty"`
	Hostname        string `json:"hostname,omitempty"`
	// StableID is the durable machine-convergent identity fingerprint
	// (nodeidentity SPKI, "sha256:<hex>"). Unlike the Headscale node id it
	// survives a churn, so it is the key the backend rebinds on (#1235 part 3).
	StableID   string `json:"stable_id,omitempty"`
	RecordedAt string `json:"recorded_at,omitempty"`
}

// identityRecordPath is the pure resolver: the record lives beside the node
// config dir, never inside network/ (so ClearState does not remove it).
func identityRecordPath(nodeConfigDir string) string {
	return filepath.Join(nodeConfigDir, identityRecordFile)
}

// SaveIdentityRecord writes the record to <nodeConfigDir>/node-identity.json.
// Pure core: nodeConfigDir is explicit so a test never touches the live node
// (the #787 GetNodeConfigDir rule). It stamps RecordedAt when unset and returns
// any error for the caller to log non-fatally.
func SaveIdentityRecord(nodeConfigDir string, rec IdentityRecord) error {
	if nodeConfigDir == "" {
		return fmt.Errorf("empty node config dir")
	}
	if rec.RecordedAt == "" {
		rec.RecordedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(nodeConfigDir, 0o755); err != nil {
		return fmt.Errorf("ensure node config dir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity record: %w", err)
	}
	// 0600: the record names the node's mesh id/IP; keep it owner-only.
	if err := os.WriteFile(identityRecordPath(nodeConfigDir), data, 0o600); err != nil {
		return fmt.Errorf("write identity record: %w", err)
	}
	return nil
}

// LoadIdentityRecord reads the record (pure core). A missing file returns
// (zero, nil): "no prior identity recorded" is the first-connect baseline, not
// an error. A corrupt file also degrades to an empty record rather than failing
// recovery, mirroring the lenient-parse posture used elsewhere in this package.
func LoadIdentityRecord(nodeConfigDir string) (IdentityRecord, error) {
	var rec IdentityRecord
	if nodeConfigDir == "" {
		return rec, nil
	}
	data, err := os.ReadFile(identityRecordPath(nodeConfigDir))
	if err != nil {
		if os.IsNotExist(err) {
			return rec, nil
		}
		return rec, fmt.Errorf("read identity record: %w", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return IdentityRecord{}, nil
	}
	return rec, nil
}

// SaveConnectedIdentity / LoadConnectedIdentity are the production wrappers that
// resolve the machine-convergent node config dir. Tests use the pure
// Save/LoadIdentityRecord with a t.TempDir() instead.
func SaveConnectedIdentity(rec IdentityRecord) error {
	return SaveIdentityRecord(GetNodeConfigDir(), rec)
}

func LoadConnectedIdentity() (IdentityRecord, error) {
	return LoadIdentityRecord(GetNodeConfigDir())
}
