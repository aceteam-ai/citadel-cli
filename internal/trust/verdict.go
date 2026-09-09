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
)

// CheckReport is one entry in the verdict's checks[] list. The DoR §3 uniform
// fields (Name/Version/Action/Severity/EvidenceHash) are what feed
// verdict_hash; Extras carries per-check human-readable data that rides the
// output map but is deliberately NOT part of verdict_hash (grounding's
// grounded/score/claims_checked live here, additive over the DoR core).
type CheckReport struct {
	Name         string
	Version      int
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
func BuildVerdict(input, output string, grounding GroundingResult, detectors []Detector) Verdict {
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
		VerdictHash: computeVerdictHash(action, checks, grounding),
	}
}

// --- verdict_hash canonicalization ---

// canonCheck is the fixed, declaration-ordered projection of a CheckReport
// that verdict_hash covers — the DoR §3 uniform check fields only. Extras are
// deliberately excluded, so adding a per-check display field never perturbs
// verdict_hash.
type canonCheck struct {
	Name         string `json:"name"`
	Version      int    `json:"version"`
	Action       string `json:"action"`
	Severity     string `json:"severity"`
	EvidenceHash string `json:"evidence_hash"`
}

// canonGrounding is the grounding block AS COVERED BY verdict_hash: the scored
// signal WITHOUT the flagged list. The DoR excludes grounding.flagged from
// verdict_hash precisely because it carries the raw evidence (the fabricated
// numbers); that evidence is bound instead through grounding's evidence_hash.
type canonGrounding struct {
	Grounded      bool    `json:"grounded"`
	Score         float64 `json:"score"`
	ClaimsChecked int     `json:"claims_checked"`
}

// canonVerdict is the DoR §3 trust_verdict object (policy_hash slots in at
// #8253 S5) as verdict_hash covers it. output_sha256 is NOT here: it is a
// receipt-level content digest, not part of the DoR's trust_verdict object,
// and it already binds content on its own.
type canonVerdict struct {
	Action    string         `json:"action"`
	Checks    []canonCheck   `json:"checks"`
	Grounding canonGrounding `json:"grounding"`
}

// computeVerdictHash returns "sha256:<hex>" over the canonical trust_verdict
// object. It uses typed structs (declaration order, explicit json tags) so
// the encoding does not depend on Go map key-sorting, mirroring
// internal/aep.Canonicalize's reason for not hashing a generic map.
//
// Float note (deliberate S6 scope): grounding.Score is encoded by json.Marshal
// (deterministic within Go for a given value), NOT with aep's fixed
// FormatFloat 'f' 6. That fixed formatting is what a SIGNED, cross-repo
// digest needs; verdict_hash here is unsigned and only needs determinism +
// sensitivity to the checks, both of which json.Marshal provides. The
// byte-exact canonical_json shared with the Python verifier is #8253 S3/S5's
// job, when verdict_hash becomes a signed field.
func computeVerdictHash(action string, checks []CheckReport, grounding GroundingResult) string {
	cc := make([]canonCheck, 0, len(checks))
	for _, c := range checks {
		cc = append(cc, canonCheck{
			Name:         c.Name,
			Version:      c.Version,
			Action:       c.Action,
			Severity:     c.Severity,
			EvidenceHash: c.EvidenceHash,
		})
	}
	cv := canonVerdict{
		Action: action,
		Checks: cc,
		Grounding: canonGrounding{
			Grounded:      grounding.Grounded,
			Score:         grounding.Score,
			ClaimsChecked: grounding.ClaimsChecked,
		},
	}
	data, err := json.Marshal(cv)
	if err != nil {
		// canonVerdict is always JSON-marshalable (no channels/funcs); this is
		// unreachable, but degrade to a stable sentinel rather than panicking
		// on the inference path.
		data = []byte("null")
	}
	return "sha256:" + hexSHA256(data)
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
