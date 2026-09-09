package whatsapp

import (
	"crypto/sha256"
	"encoding/hex"
)

// HealthPath is the bridge's own health-probe path (GET /health, per-tenant
// X-API-Key auth). It is the health_path citadel#624 Phase A reports on the
// bridge's ModuleEndpoint, sourced from this package (the bridge's own wire
// contract) rather than from the provisioned-service registry, which has no
// concept of a module's health path.
const HealthPath = "/health"

// adminKeyFingerprintHexLen is how many hex characters of the SHA-256 digest
// are kept. 16 hex chars = 64 bits, ample to detect "the key changed" without
// carrying (or coming close to reconstructing) the key itself.
const adminKeyFingerprintHexLen = 16

// AdminKeyFingerprint returns a short, one-way digest of the bridge's admin
// secret for CHANGE-OVER-TIME drift detection only (citadel#624 Phase A):
// "sha256:<first 16 hex chars>" of SHA-256(adminKey).
//
// It is deliberately NEVER the hash of an empty string: an absent/unset key
// (adminKey == "") returns "", so a control-plane consumer cannot mistake "no
// key is on disk" for a real (if practically impossible) fingerprint that
// happens to equal hash(""). "" in, "" out is the whole contract.
//
// The admin key itself never leaves this node via this or any other channel
// (whatsapp.ProvisionResult carries only the resulting api_url and the
// per-tenant data-plane key, never ADMIN_API_KEY) — the platform never learns
// the key from this fingerprint or otherwise. That means this fingerprint can
// only support detecting THAT the key rotated between two reports; it cannot
// be compared against a platform-held copy of the key, because no such copy
// exists.
func AdminKeyFingerprint(adminKey string) string {
	if adminKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(adminKey))
	return "sha256:" + hex.EncodeToString(sum[:])[:adminKeyFingerprintHexLen]
}
