// Package aep builds and signs the AEP ("AceTeam Execution Proof") receipt
// for a single job's output — v1 scope is exactly one receipt kind, the
// on-node grounding-guardrail result (internal/trust, aceteam #8253's
// guardrail half, citadel-cli#847). Signing was deferred at that merge (see
// internal/trust/grounding.go's package doc) pending a decision on which key
// backs unattended signing; docs/design-node-identity-receipts.md is that
// decision, and this package is its citadel-Go implementation.
//
// Scope boundary (see the design doc §"Scope boundary: not designing the
// Merkle-DAG"): this package signs ONE receipt — the grounding output of a
// single llm_inference job. Cross-job linking, receipt storage/retrieval,
// and DAG composition belong to aceteam's separate #8033 epic; if that epic
// lands a different canonical envelope, AEPReceiptV1 here is a candidate
// leaf node in that DAG, not a competing format.
//
// Threat model, stated honestly (design doc §"Threat model"): an on-disk
// 0600 ECDSA key with no TPM/HSM attests "this filesystem produced this
// signature," not "this specific hardware did." It protects against
// tamper-in-transit and backend-side forgery. It does NOT protect against a
// fully compromised node filesystem, and it does not prove the inference
// genuinely ran on the claimed GPU. That is what #8253's acceptance
// criterion ("verifies offline against the node's public identity") asks
// for, and no more.
package aep

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// sha256Hex returns the hex-encoded sha256 digest of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Signer is what BuildSignedReceipt needs to produce a signed AEP receipt.
// Implemented today by *nodeidentity.Store (internal/nodeidentity's ECDSA
// P-256 key) — this package deliberately does not import nodeidentity, or
// commit to any specific key backend, so a future signer (a nodevault-owned
// equivalent, or a dedicated key) can satisfy this interface without this
// package or its caller changing. See the design doc §3's identical
// interface.
type Signer interface {
	// Sign returns a signature over payload (the caller is responsible for
	// hashing/canonicalizing payload beforehand if it wants to; this
	// package's own Canonicalize already returns a fixed-shape byte
	// sequence intended to be signed directly).
	Sign(payload []byte) ([]byte, error)
	// PublicKeyFingerprint returns a stable identifier for the signer's
	// public key that a verifier can look up a registered key by.
	PublicKeyFingerprint() (string, error)
}

// AEPReceiptV1 is the signed object described in the design doc §3. Field
// order matters: Canonicalize walks the first nine fields in EXACTLY this
// order to build the signed byte sequence, so reordering fields here changes
// what gets signed (and breaks any receipt signed under the old order).
//
// Signature and PublicKeyFingerprint are populated AFTER signing and are
// deliberately excluded from Canonicalize's output — a signature that covers
// its own field is the standard way this class of scheme breaks silently
// (design doc §3).
type AEPReceiptV1 struct {
	// NodeID identifies the signing node. Prefers the fabric/platform node ID
	// (aceteam #8139, DeviceConfig.FabricNodeID) when the backend has echoed
	// one; falls back to the signer's own PublicKeyFingerprint when it
	// hasn't (see BuildSignedReceipt's nodeID parameter — resolving between
	// those two is the CALLER's job, not this package's, so this package
	// stays free of the device-config read).
	NodeID string `json:"node_id"`
	// JobID binds the signature to ONE job. Without this a valid signature
	// would be copy-pasteable onto any other job's output.
	JobID string `json:"job_id"`
	// IssuedAt is RFC3339, node-local clock.
	IssuedAt string `json:"issued_at"`
	// Engine is what actually served the request (e.g. "bonsai", "vllm").
	Engine string `json:"engine"`
	Model  string `json:"model"`

	Grounded      bool    `json:"grounded"`
	Score         float64 `json:"score"`
	ClaimsChecked int     `json:"claims_checked"`
	// FlaggedHash is sha256 (hex) of the canonical flagged-claims list, not
	// the list inline — keeps the signed payload small and fixed-shape
	// regardless of how many claims were flagged.
	FlaggedHash string `json:"flagged_hash"`

	// Signature is the base64-standard-encoded ASN.1 DER ECDSA signature
	// over Canonicalize(receipt)'s output. Empty on an unsigned receipt.
	Signature string `json:"signature,omitempty"`
	// PublicKeyFingerprint identifies the signing key (Signer.PublicKeyFingerprint).
	// A verifier looks up the node's registered public key by this value.
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
}

// Canonicalize returns a deterministic, fixed-shape byte sequence over r's
// first nine fields, in declaration order, newline-delimited. This is what
// gets hashed and signed — NOT json.Marshal(r) or json.Marshal of any
// map[string]any shape of the same data.
//
// Score (a float64) is formatted with FormatFloat's fixed-precision 'f' verb
// at 6 decimal places rather than marshaled as JSON: a value that survives a
// round-trip through a different serializer (a Redis hop, the backend's own
// re-marshal of the enclosing job-output envelope) can otherwise change byte
// representation without changing value, silently breaking a naive
// byte-signature even though nothing semantically changed (design doc §3).
// A verifier must recompute this SAME canonical form from the receipt's own
// fields to check the signature — recomputing from a generic re-marshal of
// the receipt is not equivalent unless the verifier's serializer happens to
// agree with Go's, which is exactly the assumption this function avoids
// relying on.
func Canonicalize(r *AEPReceiptV1) []byte {
	var buf bytes.Buffer
	buf.WriteString(r.NodeID)
	buf.WriteByte('\n')
	buf.WriteString(r.JobID)
	buf.WriteByte('\n')
	buf.WriteString(r.IssuedAt)
	buf.WriteByte('\n')
	buf.WriteString(r.Engine)
	buf.WriteByte('\n')
	buf.WriteString(r.Model)
	buf.WriteByte('\n')
	buf.WriteString(strconv.FormatBool(r.Grounded))
	buf.WriteByte('\n')
	buf.WriteString(strconv.FormatFloat(r.Score, 'f', 6, 64))
	buf.WriteByte('\n')
	buf.WriteString(strconv.Itoa(r.ClaimsChecked))
	buf.WriteByte('\n')
	buf.WriteString(r.FlaggedHash)
	return buf.Bytes()
}

// flaggedClaim mirrors trust.Claim's exported fields with explicit json
// tags and declaration order, so hashFlaggedClaims' input to json.Marshal is
// deterministic regardless of how trust.Claim itself is laid out.
type flaggedClaim struct {
	Value  string `json:"value"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// hashFlaggedClaims returns sha256 (hex) of the canonical JSON encoding of
// flagged, in the order trust.CheckGrounding produced it (the order claims
// were found in the output — deterministic for a given input/output pair).
func hashFlaggedClaims(flagged []trust.Claim) (string, error) {
	claims := make([]flaggedClaim, 0, len(flagged))
	for _, c := range flagged {
		claims = append(claims, flaggedClaim{Value: c.Value, Kind: string(c.Kind), Reason: c.Reason})
	}
	data, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal flagged claims: %w", err)
	}
	return sha256Hex(data), nil
}

// BuildSignedReceipt assembles an AEPReceiptV1 from a grounding result and
// signs it with signer. now is injected (rather than calling time.Now()
// internally) so tests can pin IssuedAt and the resulting signature is
// reproducible.
//
// Never returns a partially-signed receipt: either Signature and
// PublicKeyFingerprint are both populated, or an error is returned and the
// caller should not attach anything. Signing failure (e.g. the node's key
// cannot be created/read) is the caller's to handle — this package makes no
// assumption about whether that should fail the job, matching #8253's
// "never block inference" guardrail posture (see internal/trust's package
// doc); the wired caller in internal/worker treats it as fail-open.
func BuildSignedReceipt(signer Signer, nodeID, jobID, engine, model string, result trust.GroundingResult, now time.Time) (*AEPReceiptV1, error) {
	if signer == nil {
		return nil, fmt.Errorf("aep: nil signer")
	}
	flaggedHash, err := hashFlaggedClaims(result.Flagged)
	if err != nil {
		return nil, err
	}
	receipt := &AEPReceiptV1{
		NodeID:        nodeID,
		JobID:         jobID,
		IssuedAt:      now.UTC().Format(time.RFC3339),
		Engine:        engine,
		Model:         model,
		Grounded:      result.Grounded,
		Score:         result.Score,
		ClaimsChecked: result.ClaimsChecked,
		FlaggedHash:   flaggedHash,
	}

	sig, err := signer.Sign(Canonicalize(receipt))
	if err != nil {
		return nil, fmt.Errorf("sign receipt: %w", err)
	}
	fingerprint, err := signer.PublicKeyFingerprint()
	if err != nil {
		return nil, fmt.Errorf("resolve public key fingerprint: %w", err)
	}

	receipt.Signature = base64.StdEncoding.EncodeToString(sig)
	receipt.PublicKeyFingerprint = fingerprint
	return receipt, nil
}

// ToMap returns the receipt as a map[string]any (via its own json tags),
// matching the shape every other advisory receipt attached to a job's
// Output in this codebase uses (groundingReceiptMap in internal/worker,
// synthesizeReceiptFromHeaders in internal/jobs) — a job's Output crosses
// the wire through StreamWriter/Redis/API serialization elsewhere in the
// worker, where a caller attaching *AEPReceiptV1 directly (a typed Go
// pointer, the only one of its kind in that map) risks a downstream
// consumer that stringifies rather than json.Marshals the value. Callers
// MUST use this rather than attaching *AEPReceiptV1 to job output directly.
func (r *AEPReceiptV1) ToMap() (map[string]any, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("marshal receipt: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal receipt: %w", err)
	}
	return m, nil
}

// ResolveNodeID picks the receipt's node_id: the fabric/platform node ID
// when non-empty (aceteam #8139 — inert until the backend echoes one, see
// docs/design-node-identity-receipts.md §2/§4), else the signer's own
// PublicKeyFingerprint (the phasing fallback the design doc calls out in
// AEPReceiptV1's node_id field comment) so a receipt is never signed with an
// empty node_id just because #8139 hasn't landed on the wire yet.
func ResolveNodeID(signer Signer, fabricNodeID string) (string, error) {
	if fabricNodeID != "" {
		return fabricNodeID, nil
	}
	if signer == nil {
		return "", fmt.Errorf("aep: nil signer")
	}
	return signer.PublicKeyFingerprint()
}
