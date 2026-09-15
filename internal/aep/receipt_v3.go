package aep

// receipt_v3.go is the ready-but-not-default v3 receipt canon: the same fifteen
// logical fields as v2, but canonicalized with pure RFC 8785 (JCS) over the
// plain object instead of the hand-rolled newline-delimited CanonicalizeV2.
// See jcs.go for the canonicalizer and the injectivity/transport-robustness
// rationale (docs/design-canon-framework.md §7).
//
// WHY v3 exists but is not emitted: the newline canon is non-injective -- a '\n'
// inside a free-string field (engine/model are request-derived) shifts the
// field boundaries without changing the signed bytes, so one signature can
// vouch for two distinct receipts (§9.4). CanonicalizeV2 mitigates this with a
// refuse-to-sign guard (reject any field containing the delimiter). v3 removes
// the flaw at the root: JSON strings are escaped, so there is no delimiter to
// collide with -- injective by construction, no guard needed.
//
// The struct is intentionally identical in FIELDS to AEPReceiptV2 (only
// receipt_version differs) so the eventual cutover -- once the aceteam verifier
// (aceteam #9287) and its goldens move to JCS -- is a one-line flip at the
// single emission site (buildAEPReceiptV2 in internal/worker/llm_inference.go),
// not a reshaping of the receipt. Keeping v3 field-compatible is deliberate:
// the migration TRIGGER (docs/design-canon-framework.md §7.1 -- the first
// non-scalar canonical field, e.g. inline checks[]) has not landed, so v3 does
// not yet inline the verdict; it keeps verdict_hash as a scalar, exactly like
// v2.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/trust"
)

// ReceiptVersionV3 is the literal value of AEPReceiptV3.ReceiptVersion. A
// verifier branches on it (cmd/aep.go's switch, and the future aceteam
// _resolve_receipt_version "3" arm) BEFORE recomputing the canon, so it must be
// the string "3" (a JSON number would render differently in the JCS output than
// a version-detecting probe expects).
const ReceiptVersionV3 = "3"

// AEPReceiptV3 is the JCS-canonicalized receipt. Field DECLARATION ORDER is
// documentary only -- CanonicalizeV3 serializes via JCS, which sorts keys by
// UTF-16 code units, so struct order does not affect the signed bytes (unlike
// v1/v2, whose newline canon walked fields in order). The JSON tags drive both
// the wire shape and the canonical form.
//
// Score is a real JSON number here (not the v2 'f' 6 string in the verdict
// preimage): under JCS, `1` and `1.0` canonicalize identically by value, so the
// transport hop that erases Go's float-ness (a Redis/API JSON round-trip
// turning 1.0 into int 1) is harmless -- the exact hazard the 'f' 6 rule was
// introduced to dodge simply does not exist here (design §3.2 / P3).
//
// Signature and PublicKeyFingerprint are populated AFTER signing and are
// stripped before canonicalization (design R4 -- a signature that covers its
// own field is the standard silent break). They are omitempty so the stripped
// (empty) form drops them entirely from the canonical object.
type AEPReceiptV3 struct {
	ReceiptVersion string `json:"receipt_version"`
	NodeID         string `json:"node_id"`
	JobID          string `json:"job_id"`
	IssuedAt       string `json:"issued_at"`
	Engine         string `json:"engine"`
	Model          string `json:"model"`

	InputSHA256  string `json:"input_sha256"`
	OutputSHA256 string `json:"output_sha256"`
	PolicyHash   string `json:"policy_hash"`

	Action      string `json:"action"`
	VerdictHash string `json:"verdict_hash"`

	Grounded      bool    `json:"grounded"`
	Score         float64 `json:"score"`
	ClaimsChecked int     `json:"claims_checked"`
	FlaggedHash   string  `json:"flagged_hash"`

	Signature            string `json:"signature,omitempty"`
	PublicKeyFingerprint string `json:"public_key_fingerprint,omitempty"`
}

// V3Inputs carries the five v3 content/verdict fields into BuildSignedReceiptV3
// as a named struct (rather than five positional strings) so a caller cannot
// silently transpose two sha256:-shaped arguments. Structurally identical to
// V2Inputs but kept a distinct type so the v2 and v3 paths stay fully decoupled
// (the task's "keep v2 intact" invariant: a later v3-only change to this type
// can never touch v2).
type V3Inputs struct {
	InputSHA256  string
	OutputSHA256 string
	PolicyHash   string
	Action       string
	VerdictHash  string
}

// canonicalObject returns a copy of r with the two signature fields cleared, so
// CanonicalizeJCS canonicalizes exactly the signed object (design R4).
func (r *AEPReceiptV3) canonicalObject() AEPReceiptV3 {
	c := *r
	c.Signature = ""
	c.PublicKeyFingerprint = ""
	return c
}

// CanonicalizeV3 returns the RFC 8785 (JCS) canonical bytes of r, minus the two
// signature fields. This is what gets hashed and signed (and what a verifier
// recomputes from the receipt's own fields). Unlike CanonicalizeV1/V2 it can
// return an error, because JCS's implementation-defined rules are enforced here
// (non-finite floats, out-of-safe-range integers) -- see CanonicalizeJCS.
func CanonicalizeV3(r *AEPReceiptV3) ([]byte, error) {
	obj := r.canonicalObject()
	return CanonicalizeJCS(&obj)
}

// VerdictHashV3 computes the v3 verdict_hash: "sha256:<hex>" over the SAME
// JCS canonicalizer CanonicalizeV3 uses (design R5 -- one canonicalizer for the
// receipt AND the verdict object). It reuses trust.VerdictHashPreimage
// unchanged, so the preimage OBJECT is byte-identical to what v2 computes
// verdict_hash over; only the CANONICALIZER changes (JCS here vs the
// hand-ported canonical_json in trust for v2). The eventual Python cutover is
// therefore `rfc8785.dumps(same_preimage_dict)`, not a reshaping of the
// preimage.
//
// (The preimage carries score as its 'f' 6 string, which is inert under JCS --
// it is just a string there. The 1-vs-1.0 number property is exercised on the
// receipt's own score field and in the conformance corpus, not inside this hash
// preimage.)
func VerdictHashV3(action string, checks []trust.CheckReport, grounding trust.GroundingResult, policyHash string) (string, error) {
	canon, err := CanonicalizeJCS(trust.VerdictHashPreimage(action, checks, grounding, policyHash))
	if err != nil {
		return "", fmt.Errorf("aep: canonicalize verdict preimage: %w", err)
	}
	return "sha256:" + sha256Hex(canon), nil
}

// BuildSignedReceiptV3 assembles an AEPReceiptV3 and signs it with signer. now
// is injected so tests can pin IssuedAt. Never returns a partially-signed
// receipt: either Signature and PublicKeyFingerprint are both set, or an error
// is returned. Mirrors BuildSignedReceiptV2's fail-open contract (the wired
// caller treats a returned error as fail-open -- the job still succeeds without
// a receipt).
//
// NO newline refuse-to-sign guard (unlike BuildSignedReceiptV2): JCS escapes
// string contents, so a '\n' in engine/model can never collide field
// boundaries -- the canon is injective by construction, which is the entire
// point of v3. The one emission-time guard that remains is the non-finite score
// check below; the integer safe-range check lives inside CanonicalizeV3.
func BuildSignedReceiptV3(signer Signer, nodeID, jobID, engine, model string, in V3Inputs, result trust.GroundingResult, now time.Time) (*AEPReceiptV3, error) {
	if signer == nil {
		return nil, fmt.Errorf("aep: nil signer")
	}
	if math.IsNaN(result.Score) || math.IsInf(result.Score, 0) {
		return nil, fmt.Errorf("aep: score %v is not finite; refusing to sign", result.Score)
	}
	flaggedHash, err := hashFlaggedClaims(result.Flagged)
	if err != nil {
		return nil, err
	}
	receipt := &AEPReceiptV3{
		ReceiptVersion: ReceiptVersionV3,
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

	canon, err := CanonicalizeV3(receipt)
	if err != nil {
		return nil, fmt.Errorf("aep: canonicalize v3 receipt: %w", err)
	}
	sig, err := signer.Sign(canon)
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
// same shape rule as AEPReceiptV1.ToMap / AEPReceiptV2.ToMap -- callers MUST use
// this rather than attaching *AEPReceiptV3 to job output directly (a typed Go
// pointer in a map that crosses the wire is the hazard ToMap avoids).
func (r *AEPReceiptV3) ToMap() (map[string]any, error) {
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
