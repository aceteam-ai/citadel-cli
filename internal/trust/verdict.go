package trust

// This file assembles a Trust Engine verdict (aceteam #8253 DoR §3) from the
// grounding result plus the pure detectors in detectors.go, and computes the
// unsigned verdict_hash. It is pure and deterministic — no I/O, no clock — so
// the whole assembly (including the mutation test that drops a detector and
// asserts verdict_hash changes) is unit-testable in this package.
//
// What this is NOT: it is not the signed AEP receipt. verdict_hash here is an
// UNSIGNED fixity digest attached alongside output_sha256, same posture — it
// lets a consumer confirm which verdict it holds without the signed receipt.
// #8253 S3 owns the SIGNED AEPReceiptV2 (which adds verdict_hash as a signed
// canonical field) and the byte-exact cross-repo canonical_json the Python
// verifier recomputes; this package deliberately does not couple to that yet
// (see computeVerdictHash's note on float formatting).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// CheckReport is one entry in the verdict's checks[] list. The DoR §3 uniform
// fields (Name/Version/Action/Severity/EvidenceHash) are what feed
// verdict_hash; Extras carries per-check human-readable data that rides the
// output map but is deliberately NOT part of verdict_hash (grounding's
// grounded/score/claims_checked live here, additive over the DoR core).
type CheckReport struct {
	Name    string
	Version int
	// Action is "pass" or "flag" for this individual check. (block is #8253
	// S5's policy-driven posture and is not produced on-node yet.)
	Action string
	// Severity is the highest severity among the check's findings, or "" when
	// it passed. Grounding is a scored check with no per-finding severity, so
	// its Severity is always "".
	Severity string
	// EvidenceHash is the bare-hex sha256 of the check's canonical evidence
	// list — never the evidence itself (DoR §3). It uses the SAME shape and
	// bare-hex encoding as internal/aep's flagged_hash, so grounding's
	// EvidenceHash equals a signed receipt's flagged_hash for the same input.
	EvidenceHash string
	// Extras are additive per-check fields for the output map only. Nil for
	// the detector checks; grounding carries grounded/score/claims_checked.
	Extras map[string]any
}

// Map renders a CheckReport as the map[string]any that rides the verdict
// output. The DoR core fields first, then any Extras.
func (c CheckReport) Map() map[string]any {
	m := map[string]any{
		"name":          c.Name,
		"version":       c.Version,
		"action":        c.Action,
		"severity":      c.Severity,
		"evidence_hash": c.EvidenceHash,
	}
	for k, v := range c.Extras {
		m[k] = v
	}
	return m
}

// Verdict is the assembled Trust Engine result for one (input, output) pair:
// the aggregate action, the ordered checks, and the unsigned verdict_hash.
type Verdict struct {
	// Action is the aggregate: "flag" if grounding is ungrounded OR any
	// detector flagged, else "pass". Never "block" on-node today (see S5).
	Action string
	// Checks is grounding first, then the detectors in the order they were
	// passed to BuildVerdict.
	Checks []CheckReport
	// VerdictHash is "sha256:<hex>" over the DoR §3 canonical trust_verdict
	// object (excluding grounding.flagged). Unsigned.
	VerdictHash string
}

// CheckMaps is the checks[] list as []map[string]any for the output map.
func (v Verdict) CheckMaps() []map[string]any {
	out := make([]map[string]any, 0, len(v.Checks))
	for _, c := range v.Checks {
		out = append(out, c.Map())
	}
	return out
}

// BuildVerdict runs grounding (already computed and passed in) plus each
// detector over (input, output), and assembles the ordered checks, aggregate
// action, and verdict_hash. detectors is passed explicitly (see
// DefaultDetectors) so the set is a seam, not a hardcode: dropping a detector
// removes its checks[] entry AND changes verdict_hash, which is exactly what
// the mutation test asserts.
//
// policyHash is the "sha256:<hex>" policy digest that rides the verdict_hash
// preimage's top-level policy_hash field (aceteam #8253 S3, DoR §3 / design
// §3.5). The caller passes aep.EmptyPolicyHash today (the hash of the empty
// policy {}, since S4/S5 policy delivery has not landed); it is a parameter,
// not a constant here, so this leaf package never imports internal/aep and S5
// can substitute a real received-policy hash without touching this signature.
func BuildVerdict(input, output string, grounding GroundingResult, detectors []Detector, policyHash string) Verdict {
	checks := make([]CheckReport, 0, len(detectors)+1)

	groundingAction := "pass"
	if !grounding.Grounded {
		groundingAction = "flag"
	}
	checks = append(checks, CheckReport{
		Name:    "grounding",
		Version: 1,
		Action:  groundingAction,
		// A scored check, not a severity-graded one: its graded signal
		// (grounded/score/claims_checked) lives in Extras and in the separate
		// top-level "grounding" block, so Severity stays "".
		Severity:     "",
		EvidenceHash: hashGroundingEvidence(grounding.Flagged),
		Extras: map[string]any{
			"grounded":       grounding.Grounded,
			"score":          grounding.Score,
			"claims_checked": grounding.ClaimsChecked,
		},
	})

	flagged := !grounding.Grounded
	for _, d := range detectors {
		findings := d.Check(input, output)
		action := "pass"
		severity := ""
		if len(findings) > 0 {
			action = "flag"
			severity = maxSeverity(findings)
			flagged = true
		}
		checks = append(checks, CheckReport{
			Name:         d.Name(),
			Version:      d.Version(),
			Action:       action,
			Severity:     severity,
			EvidenceHash: hashFindings(findings),
		})
	}

	action := "pass"
	if flagged {
		action = "flag"
	}
	return Verdict{
		Action:      action,
		Checks:      checks,
		VerdictHash: computeVerdictHash(action, checks, grounding, policyHash),
	}
}

// --- verdict_hash canonicalization ---
//
// The verdict_hash preimage is the DoR §3.5 canonical trust_verdict object,
// rendered by the aceteam-aep `canonical_json` algorithm (sorted keys at every
// depth, compact separators, ensure_ascii, drop null map values). This is a
// SIGNED cross-repo contract at #8253 S3: a future aceteam/aceteam-aep verifier
// recomputes verdict_hash from the receipt's fields and must derive the same
// bytes, so this is a faithful port of that specific canonicalizer, NOT
// json.Marshal (which sorts keys but HTML-escapes <>& and emits raw UTF-8 for
// non-ASCII — both divergences from Python's ensure_ascii json.dumps).
//
// The preimage carries `score` as its FormatFloat 'f' 6 STRING (design §3.4
// option A), so the object contains no floats at all — the one float rule in
// the whole receipt system (a score is rendered 'f' 6) is reused rather than a
// second one invented, and the canonicalizer can reject float64 outright as a
// guardrail.

// computeVerdictHash returns "sha256:<hex>" over canonicalJSON of the DoR §3.5
// verdict preimage:
//
//	{
//	  "action":      "<pass|flag|block>",
//	  "checks":      [ {"action","evidence_hash","name","severity","version"} ... ],
//	  "grounding":   {"claims_checked","grounded","score":"<'f' 6 string>"},
//	  "policy_hash": "sha256:<hex>"
//	}
//
// Excluded, per the DoR: grounding.flagged (raw evidence — bound through the
// per-check evidence_hash), each check's Extras (display-only), and the
// receipt-level output_sha256/verdict_hash themselves (a hash never covers its
// own field). Check list order is the node's own (lists are not sorted).
func computeVerdictHash(action string, checks []CheckReport, grounding GroundingResult, policyHash string) string {
	data, err := canonicalJSON(VerdictHashPreimage(action, checks, grounding, policyHash))
	if err != nil {
		// The preimage is built from only string/bool/int/map/list, so
		// canonicalJSON never errors on it; degrade to a stable sentinel rather
		// than panicking on the inference path if a future edit adds a float.
		data = []byte("null")
	}
	return "sha256:" + hexSHA256(data)
}

// VerdictHashPreimage returns the exact object verdict_hash is computed over
// (design §3.5), as a map[string]any: sorted-key-canonicalized by canonicalJSON
// and hashed by computeVerdictHash. It is exported so the cross-repo golden
// fixture (internal/aep/testdata/v2) can serialize it for a future aceteam/
// aceteam-aep verifier to reproduce with its own canonical_json.
//
// score is carried as its FormatFloat 'f' 6 STRING (design §3.4 option A), so
// the object contains no float — the canonicalizer needs no float branch and
// both repos reuse the one score-rendering rule the receipt canon already has.
func VerdictHashPreimage(action string, checks []CheckReport, grounding GroundingResult, policyHash string) map[string]any {
	checksList := make([]any, 0, len(checks))
	for _, c := range checks {
		checksList = append(checksList, map[string]any{
			"name":          c.Name,
			"version":       c.Version,
			"action":        c.Action,
			"severity":      c.Severity,
			"evidence_hash": c.EvidenceHash,
		})
	}
	return map[string]any{
		"action": action,
		"checks": checksList,
		"grounding": map[string]any{
			"grounded":       grounding.Grounded,
			"score":          strconv.FormatFloat(grounding.Score, 'f', 6, 64),
			"claims_checked": grounding.ClaimsChecked,
		},
		"policy_hash": policyHash,
	}
}

// CanonicalJSON exposes the aep canonical_json port (see canonicalJSON) for the
// cross-repo golden and for tests. It reproduces aceteam-aep's canonical_json
// byte-for-byte for the JSON value shapes the verdict preimage uses.
func CanonicalJSON(v any) ([]byte, error) {
	return canonicalJSON(v)
}

// canonicalJSON reproduces aceteam-aep's `canonical_json` (aceteam-aep
// src/aceteam_aep/attestation.py) byte-for-byte for the value shapes the
// verdict preimage uses: keys sorted at every depth, compact `,`/`:`
// separators, ensure_ascii escaping, and null values dropped from maps (but
// kept in lists). It rejects float64 outright — the preimage carries score as
// a string, so any float64 reaching here is a bug, and refusing it is a
// guardrail rather than a silent cross-language divergence (Go and Python
// render floats differently, e.g. 1.0 -> "1" vs "1.0").
func canonicalJSON(v any) ([]byte, error) {
	var b strings.Builder
	if err := encodeCanonical(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func encodeCanonical(b *strings.Builder, v any) error {
	switch val := v.(type) {
	case nil:
		// Reached only for a nil inside a list; a nil map value is dropped by
		// the map branch before recursing. Python renders it as null.
		b.WriteString("null")
	case string:
		encodeCanonicalString(b, val)
	case bool:
		if val {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int:
		b.WriteString(strconv.Itoa(val))
	case int64:
		b.WriteString(strconv.FormatInt(val, 10))
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k, mv := range val {
			if mv == nil { // Python's _clean drops None from dicts.
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			encodeCanonicalString(b, k)
			b.WriteByte(':')
			if err := encodeCanonical(b, val[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := encodeCanonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		return fmt.Errorf("trust: canonicalJSON: unsupported type %T (float64 is deliberately rejected — carry it as a string)", v)
	}
	return nil
}

// encodeCanonicalString writes s as a JSON string escaped exactly as Python's
// json.dumps(..., ensure_ascii=True) does: the short escapes for \" \\ \n \r
// \t \b \f, other control characters and 0x7f and all non-ASCII as lowercase
// \uXXXX (non-BMP as UTF-16 surrogate pairs), everything in 0x20..0x7e raw.
// Notably it does NOT HTML-escape < > &, which Go's encoding/json would.
func encodeCanonicalString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r >= 0x20 && r <= 0x7e {
				b.WriteRune(r)
			} else if r > 0xffff {
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, r1, r2)
			} else {
				fmt.Fprintf(b, `\u%04x`, r)
			}
		}
	}
	b.WriteByte('"')
}

// --- evidence hashing ---

// groundingEvidence mirrors internal/aep's flaggedClaim shape ({value, kind,
// reason}, that declaration order) so grounding's evidence_hash is
// byte-identical to a signed receipt's flagged_hash for the same flagged list.
type groundingEvidence struct {
	Value  string `json:"value"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// hashGroundingEvidence returns the bare-hex sha256 of the canonical
// flagged-claims list (empty list -> hash of "[]", matching aep). Bare hex
// (no "sha256:" prefix) to match aep.flagged_hash's encoding.
func hashGroundingEvidence(flagged []Claim) string {
	items := make([]groundingEvidence, 0, len(flagged))
	for _, c := range flagged {
		items = append(items, groundingEvidence{Value: c.Value, Kind: string(c.Kind), Reason: c.Reason})
	}
	data, err := json.Marshal(items)
	if err != nil {
		data = []byte("[]")
	}
	return hexSHA256(data)
}

// hashFindings returns the bare-hex sha256 of a detector's canonical findings
// list (empty list -> hash of "[]", consistent with hashGroundingEvidence).
func hashFindings(findings []Finding) string {
	items := findings
	if items == nil {
		items = []Finding{}
	}
	data, err := json.Marshal(items)
	if err != nil {
		data = []byte("[]")
	}
	return hexSHA256(data)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
