package aep

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// ReceiptVersionV2 is the literal value of AEPReceiptV2.ReceiptVersion and the
// first field of the v2 canonical form. The verifier's _resolve_receipt_version
// branches on the string "2" (aceteam python-backend/utils/aep_receipt_verify.py,
// issue #8253 S2 / #9287); emitting the string (not a JSON number) keeps the
// wire value and the canonical rendering identical.
const ReceiptVersionV2 = "2"

// EmptyPolicyHash is the interim policy_hash a v2 receipt carries whenever the
// job payload declares no policy (aceteam #8253 S3, docs/design-trust-receipt-v2.md
// §2.3, DoR §7.4 ratified 2026-09-07): "sha256:" + hex(sha256("{}")).
//
// It is canonicalizer-independent — every JSON canonicalizer renders the empty
// object as the two bytes {} — so the node can emit it before the Go
// canonical_json port for a real policy (S5) exists, and a verifier supplied
// this same value for expected_policy_hash resolves policy_bound: true. S5
// replaces the SOURCE of this value (the hash of the actually-received policy
// bytes); the shape (a sha256:-prefixed digest at the receipt's policy_hash
// field, and inside the verdict_hash preimage) does not change again.
//
// Pinned equal to "sha256:" + hex(sha256("{}")) by TestEmptyPolicyHash so the
// literal below can never drift from its definition.
const EmptyPolicyHash = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

// AEPReceiptV2 is the fifteen-field signed receipt (aceteam #8253 S2/S3, DoR §3,
// 2026-09-06 delta). It extends AEPReceiptV1 with content binding
// (input_sha256/output_sha256/policy_hash) and the Trust Engine verdict
// (action/verdict_hash), and carries receipt_version FIRST so a verifier
// branches on it before recomputing the rest.
//
// Field DECLARATION ORDER here is documentary only — CanonicalizeV2 walks the
// v2CanonicalFields table, not the struct, so the signed bytes come from that
// table's order (which is asserted against testdata/v2/fields.txt). The JSON
// tags drive ToMap()'s wire shape.
//
// Signature and PublicKeyFingerprint are populated AFTER signing and are
// deliberately excluded from the canonical bytes (a signature that covers its
// own field is the standard silent break — same rule as v1).
type AEPReceiptV2 struct {
	// ReceiptVersion is always ReceiptVersionV2 ("2"). First canonical field.
	ReceiptVersion string `json:"receipt_version"`
	NodeID         string `json:"node_id"`
	JobID          string `json:"job_id"`
	IssuedAt       string `json:"issued_at"`
	Engine         string `json:"engine"`
	Model          string `json:"model"`

	// InputSHA256/OutputSHA256/PolicyHash are content-binding digests, each a
	// "sha256:<64 lowercase hex>" string opaque to the canon (the verifier
	// string-compares them against caller-supplied expected values; they are
	// not recomputed as part of the signature check).
	InputSHA256  string `json:"input_sha256"`
	OutputSHA256 string `json:"output_sha256"`
	PolicyHash   string `json:"policy_hash"`

	// Action is the Trust Engine aggregate verdict ("pass"|"flag"|"block";
	// "block" cannot occur before S5). VerdictHash is "sha256:<hex>" over the
	// §3.5 verdict preimage.
	Action      string `json:"action"`
	VerdictHash string `json:"verdict_hash"`

	// Grounded/Score/ClaimsChecked/FlaggedHash are carried verbatim from v1
	// (same rendering rules; FlaggedHash stays BARE hex, no sha256: prefix).
	Grounded      bool    `json:"grounded"`
	Score         float64 `json:"score"`
	ClaimsChecked int     `json:"claims_checked"`
	FlaggedHash   string  `json:"flagged_hash"`

	Signature            string `json:"signature,omitempty"`
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
}

// v2Field pairs a canonical field's name (the fields.txt / _CANONICAL_FIELDS_V2
// identity) with the function that renders its canonical string.
type v2Field struct {
	name   string
	render func(*AEPReceiptV2) string
}

// v2CanonicalFields is THE authority for the v2 canonical form: the fifteen
// fields, in order, each with its exact rendering rule. It must reproduce
// aceteam's canonicalize_receipt_v2 byte-for-byte (grounded via FormatBool,
// score via FormatFloat 'f' 6, claims_checked via Itoa, every other field as
// its plain string). CanonicalizeV2 and the refuse-to-sign guard both walk
// this table, and TestV2CanonicalFieldsMatchTestdata asserts its names equal
// internal/aep/testdata/v2/fields.txt — the cross-repo drift guard (fable's
// zero-cost SST, design §12.5 / canon-framework §2).
var v2CanonicalFields = []v2Field{
	{"receipt_version", func(r *AEPReceiptV2) string { return r.ReceiptVersion }},
	{"node_id", func(r *AEPReceiptV2) string { return r.NodeID }},
	{"job_id", func(r *AEPReceiptV2) string { return r.JobID }},
	{"issued_at", func(r *AEPReceiptV2) string { return r.IssuedAt }},
	{"engine", func(r *AEPReceiptV2) string { return r.Engine }},
	{"model", func(r *AEPReceiptV2) string { return r.Model }},
	{"input_sha256", func(r *AEPReceiptV2) string { return r.InputSHA256 }},
	{"output_sha256", func(r *AEPReceiptV2) string { return r.OutputSHA256 }},
	{"policy_hash", func(r *AEPReceiptV2) string { return r.PolicyHash }},
	{"action", func(r *AEPReceiptV2) string { return r.Action }},
	{"verdict_hash", func(r *AEPReceiptV2) string { return r.VerdictHash }},
	{"grounded", func(r *AEPReceiptV2) string { return strconv.FormatBool(r.Grounded) }},
	{"score", func(r *AEPReceiptV2) string { return strconv.FormatFloat(r.Score, 'f', 6, 64) }},
	{"claims_checked", func(r *AEPReceiptV2) string { return strconv.Itoa(r.ClaimsChecked) }},
	{"flagged_hash", func(r *AEPReceiptV2) string { return r.FlaggedHash }},
}

// V2CanonicalFieldNames returns the ordered canonical field names — the list
// pinned by fields.txt (exported so a test can assert the drift guard).
func V2CanonicalFieldNames() []string {
	names := make([]string, len(v2CanonicalFields))
	for i, f := range v2CanonicalFields {
		names[i] = f.name
	}
	return names
}

// CanonicalizeV2 returns the deterministic, newline-delimited byte sequence over
// r's fifteen canonical fields, in v2CanonicalFields order, no trailing newline,
// UTF-8. This is what gets hashed and signed. It reproduces aceteam's
// canonicalize_receipt_v2 exactly.
//
// Deliberately TOTAL: it never rejects an input (the refuse-to-sign guard lives
// in BuildSignedReceiptV2, not here), because the Python verifier's canonicalizer
// is also total — a verifier must be able to recompute the canon of ANY receipt
// it receives to check the signature. See BuildSignedReceiptV2 for the guard.
func CanonicalizeV2(r *AEPReceiptV2) []byte {
	parts := make([]string, len(v2CanonicalFields))
	for i, f := range v2CanonicalFields {
		parts[i] = f.render(r)
	}
	return []byte(strings.Join(parts, "\n"))
}

// V2Inputs carries the five v2-only content/verdict fields into
// BuildSignedReceiptV2 as a named struct (rather than five positional strings)
// so a caller cannot silently transpose two sha256:-shaped arguments.
type V2Inputs struct {
	InputSHA256  string
	OutputSHA256 string
	PolicyHash   string
	Action       string
	VerdictHash  string
}

// BuildSignedReceiptV2 assembles an AEPReceiptV2 and signs it with signer. now
// is injected so tests can pin IssuedAt. Never returns a partially-signed
// receipt: either Signature and PublicKeyFingerprint are both set, or an error
// is returned. Mirrors BuildSignedReceipt's fail-open contract (the wired caller
// in internal/worker treats a returned error as fail-open — the job still
// succeeds without a receipt).
//
// REFUSE-TO-SIGN GUARD (owner decision, docs/design-trust-receipt-v2.md; closes
// the non-injectivity of the newline-delimited canon that
// docs/design-canon-framework.md §2 R1.1 demonstrates): the canon uses '\n' as
// its field delimiter and writes free-string fields raw, so engine="bonsai\nx",
// model="y" and engine="bonsai", model="x\ny" canonicalize to the identical
// bytes — one signature would then vouch for two distinct receipts. This refuses
// to sign ANY receipt whose rendered canonical fields contain the delimiter,
// before signing. It changes NO valid-input bytes (a value with no newline is
// unaffected), so it stays byte-compatible with the deployed verifier; it only
// rejects the colliding inputs. Model and engine are request/payload-derived, so
// this is untrusted input, not merely node-controlled.
func BuildSignedReceiptV2(signer Signer, nodeID, jobID, engine, model string, in V2Inputs, result trust.GroundingResult, now time.Time) (*AEPReceiptV2, error) {
	if signer == nil {
		return nil, fmt.Errorf("aep: nil signer")
	}
	flaggedHash, err := hashFlaggedClaims(result.Flagged)
	if err != nil {
		return nil, err
	}
	receipt := &AEPReceiptV2{
		ReceiptVersion: ReceiptVersionV2,
		NodeID:         nodeID,
		JobID:          jobID,
		IssuedAt:       now.UTC().Format(time.RFC3339),
		Engine:         engine,
		Model:          model,
		InputSHA256:    in.InputSHA256,
		OutputSHA256:   in.OutputSHA256,
		PolicyHash:     in.PolicyHash,
		Action:         in.Action,
		VerdictHash:    in.VerdictHash,
		Grounded:       result.Grounded,
		Score:          result.Score,
		ClaimsChecked:  result.ClaimsChecked,
		FlaggedHash:    flaggedHash,
	}

	for _, f := range v2CanonicalFields {
		if strings.ContainsRune(f.render(receipt), '\n') {
			return nil, fmt.Errorf(
				"aep: field %q contains the canonical delimiter (newline); refusing to sign a non-injective receipt",
				f.name,
			)
		}
	}

	sig, err := signer.Sign(CanonicalizeV2(receipt))
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

// ToMap returns the receipt as a map[string]any (via its own json tags), the
// same shape rule as AEPReceiptV1.ToMap — callers MUST use this rather than
// attaching *AEPReceiptV2 to job output directly (a typed Go pointer in a map
// that crosses the wire is the hazard ToMap avoids).
func (r *AEPReceiptV2) ToMap() (map[string]any, error) {
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
