// Package trust implements on-node, local guardrails that check a model's
// output against its input BEFORE the result leaves the node. It runs no
// network calls and makes no central round-trip — the check is a pure
// function of two strings, so it stays fast and fully unit-testable.
//
// v1 (citadel #8253, guardrail half) ships exactly one guardrail:
// CheckGrounding, which flags numeric/statistic claims in a model's output
// that are not supported by (present in, or cheaply derivable from) its
// input. It exists because an insight-extraction run on llama3.1:8b turned
// the source phrases "a majority" / "a small fraction" into fabricated
// numbers "68%" / "7%" — a hallucination a purely syntactic check can catch
// without an LLM judge.
//
// This package deliberately does NOT sign, persist, or transmit anything.
// Attaching a GroundingResult to a job's output/receipt, and deciding
// whether to sign that receipt, is the caller's responsibility — see
// internal/worker/llm_inference.go's bufferedChatCompletions for the one
// wired integration point. Cryptographically signing the receipt (the AEP
// half of #8253, internal/aep) is now implemented — see
// docs/design-node-identity-receipts.md for the design and
// internal/aep's package doc for the implementation. It signs with
// internal/nodeidentity's ECDSA key, NOT internal/nodevault: nodevault is a
// PIN-gated symmetric secrets vault that structurally cannot back unattended
// signing under a headless `citadel work` (see the design doc §1c). This
// package (internal/trust) still does not sign anything itself — the
// signing lives entirely in internal/aep and its call site.
package trust

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ClaimKind classifies a numeric token found in a model's output. Kind
// decides ELIGIBILITY: classify first, then only eligible kinds are checked
// for support and can appear in GroundingResult.Flagged or count toward
// Score's denominator. See eligible() for the exact rule per kind.
type ClaimKind string

const (
	// ClaimPercent is "N%", "N percent", or "N pct" — always eligible. This is
	// the kind that makes the motivating incident (68%/7% fabricated from "a
	// majority"/"a small fraction") flag: a bare percent claim with nothing
	// numeric behind it in the input has no support to find.
	ClaimPercent ClaimKind = "percent"
	// ClaimRatio is "N out of M" — always eligible. Support additionally
	// accepts the input stating the same ratio, or the derived N/M*100
	// percentage appearing in the input.
	ClaimRatio ClaimKind = "ratio"
	// ClaimCurrency is "$N" or "N dollars"/"N USD" — always eligible.
	ClaimCurrency ClaimKind = "currency"
	// ClaimCount is any other bare number not classified as one of the
	// ineligible kinds below — eligible.
	ClaimCount ClaimKind = "count"
	// ClaimYear is a bare 4-digit number in [1900, 2100] with no surrounding
	// percent/currency/ratio marker — INELIGIBLE (skipped). A year is not a
	// "statistic" in the sense this guardrail targets, and treating every
	// mention of "2026" as a claim needing input support would drown the
	// signal in false positives.
	ClaimYear ClaimKind = "year"
	// ClaimOrdinal is a list index or ordinal ("1.", "2)", "3rd") —
	// INELIGIBLE (skipped). These enumerate structure, not facts about the
	// world, so they carry nothing to fabricate.
	ClaimOrdinal ClaimKind = "ordinal"
)

// eligible reports whether claims of this kind are checked against the input
// at all. Ineligible kinds (year, ordinal) are extracted only so their span
// of text is not later misclassified as a count — they never appear in
// GroundingResult.Flagged and never count toward Score's denominator.
func eligible(k ClaimKind) bool {
	switch k {
	case ClaimYear, ClaimOrdinal:
		return false
	default:
		return true
	}
}

// Claim is one numeric/statistic mention in the output, its classification,
// and — for a flagged claim — why it was flagged.
type Claim struct {
	// Value is the claim's literal text as it appeared in the output (e.g.
	// "68%", "7 out of 100", "$42").
	Value string
	// Kind is the claim's classification. See ClaimKind.
	Kind ClaimKind
	// Reason explains why the claim was flagged. Empty for a claim that was
	// found supported (Claim only appears in GroundingResult.Flagged when
	// unsupported, so Reason is always populated there).
	Reason string
}

// Policy decides what a caller does with an unsupported claim. CheckGrounding
// itself never blocks anything — it only classifies and reports; Policy is
// read by callers deciding whether to gate. The zero value is PolicyFlag, so
// flag-only is the default by construction, not by a config lookup.
type Policy int

const (
	// PolicyFlag attaches GroundingResult to the output/receipt but never
	// blocks. Default.
	PolicyFlag Policy = iota
	// PolicyGate additionally withholds/HITL-queues an ungrounded result.
	// Off by default; a caller must opt in explicitly.
	PolicyGate
)

// GroundingResult is the outcome of CheckGrounding.
type GroundingResult struct {
	// Grounded is true iff Flagged is empty. An output with ZERO eligible
	// claims is Grounded==true with Score==1.0 — this reports "nothing
	// numeric was found unsupported", not "this output is true". A
	// prose-only answer with no statistics scores a perfect 1.0 while being
	// entirely unverified by this guardrail; callers must not read Score as
	// a truthfulness score.
	Grounded bool
	// Score is (eligible claims found supported) / (total eligible claims).
	// Score is 1.0 when there are no eligible claims at all (vacuously
	// grounded — see the Grounded field comment for why that is not the
	// same as "verified true").
	Score float64
	// ClaimsChecked is the eligible-claim denominator behind Score — the
	// count of numeric/statistic claims this guardrail actually evaluated
	// (percent/ratio/currency/count; excludes years and list indices, see
	// ClaimKind). A caller MUST read this alongside Score/Grounded: a
	// claim-free reply and a reply with ten verified statistics both report
	// Grounded=true, Score=1.0, but ClaimsChecked distinguishes 0 from 10 —
	// see the Grounded field comment for why Score alone is not a
	// truthfulness signal.
	ClaimsChecked int
	// Flagged lists every eligible claim that had no support in the input,
	// in the order it was found in the output.
	Flagged []Claim
}

// Block reports whether p directs the caller to withhold/gate r. Gating is
// opt-in: PolicyFlag (the zero value) never blocks, regardless of r.
func (r GroundingResult) Block(p Policy) bool {
	return p == PolicyGate && !r.Grounded
}

// --- extraction ---

// numFrag matches an integer or decimal number, optionally with thousands
// separators (e.g. "1,234", "68", "7.5"). It is the numeric core every claim
// regex below is built from.
const numFrag = `\d[\d,]*(?:\.\d+)?`

var (
	ratioRe = regexp.MustCompile(`(?i)(` + numFrag + `)\s+out of\s+(` + numFrag + `)`)
	// The \b must sit INSIDE the alternation, after the word-form spellings
	// only: "%" is a non-word char, so a \b immediately after it never
	// matches when followed by another non-word char (space, punctuation) —
	// there is no word/non-word transition to anchor on. Requiring \b after
	// the whole alternation silently made this regex never match "68%".
	percentRe   = regexp.MustCompile(`(?i)(` + numFrag + `)\s*(?:%|(?:percent|pct\.?)\b)`)
	currencyRe  = regexp.MustCompile(`(?i)[$€£]\s*(` + numFrag + `)|(` + numFrag + `)\s*(?:dollars|usd)\b`)
	ordinalRe   = regexp.MustCompile(`(?i)\b(\d+)\s*(?:st|nd|rd|th)\b`)
	listIndexRe = regexp.MustCompile(`(?m)^\s*(\d{1,3})[.)]\s+`)
	yearRe      = regexp.MustCompile(`\b((?:19|20)\d{2})\b`)
	// countRe must capture the number in group 1 — addFromGroups only reads
	// explicitly listed groups, so an ungrouped whole-match pattern here
	// would silently drop every bare-number match (len(nums)==0 -> skipped).
	countRe = regexp.MustCompile(`(` + numFrag + `)`)
)

// span is a byte range [start, end) in the scanned text, used to prevent a
// higher-priority claim (e.g. a percent) from being re-matched as a lower
// priority one (a bare count).
type span struct{ start, end int }

func (s span) overlaps(o span) bool { return s.start < o.end && o.start < s.end }

func overlapsAny(s span, spans []span) bool {
	for _, o := range spans {
		if s.overlaps(o) {
			return true
		}
	}
	return false
}

// rawClaim is an extracted claim plus its parsed numeric value(s), used
// internally before it is reduced to the public Claim shape.
type rawClaim struct {
	text string
	kind ClaimKind
	span span
	num  float64
	// num2 is the second operand of a ratio ("N out of M" -> num=N, num2=M).
	num2 *float64
}

// extractClaims scans text and returns every numeric claim found, in
// left-to-right order, classified by kind. Priority (highest first) is
// ratio > percent > currency > ordinal > list index > year > bare count: a
// span already claimed by a higher-priority match is never re-matched by a
// lower-priority one (e.g. the "68" in "68%" is not also reported as a bare
// count).
func extractClaims(text string) []rawClaim {
	var claims []rawClaim
	var claimed []span

	addFromGroups := func(re *regexp.Regexp, kind ClaimKind, groupIdx ...int) {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			sp := span{m[0], m[1]}
			if overlapsAny(sp, claimed) {
				continue
			}
			var nums []float64
			ok := true
			for _, gi := range groupIdx {
				lo, hi := m[2*gi], m[2*gi+1]
				if lo < 0 {
					continue
				}
				n, err := parseNumber(text[lo:hi])
				if err != nil {
					ok = false
					break
				}
				nums = append(nums, n)
			}
			if !ok || len(nums) == 0 {
				continue
			}
			rc := rawClaim{text: strings.TrimSpace(text[sp.start:sp.end]), kind: kind, span: sp, num: nums[0]}
			if len(nums) > 1 {
				n2 := nums[1]
				rc.num2 = &n2
			}
			claims = append(claims, rc)
			claimed = append(claimed, sp)
		}
	}

	addFromGroups(ratioRe, ClaimRatio, 1, 2)
	addFromGroups(percentRe, ClaimPercent, 1)
	// currencyRe has two alternative groups ($N vs N dollars); only one is
	// populated per match.
	addFromGroups(currencyRe, ClaimCurrency, 1, 2)
	addFromGroups(ordinalRe, ClaimOrdinal, 1)
	addFromGroups(listIndexRe, ClaimOrdinal, 1)
	addFromGroups(yearRe, ClaimYear, 1)
	addFromGroups(countRe, ClaimCount, 1)

	sort.Slice(claims, func(i, j int) bool { return claims[i].span.start < claims[j].span.start })
	return claims
}

// parseNumber normalizes a matched numeric token (stripping thousands
// separators) to a float64.
func parseNumber(s string) (float64, error) {
	return strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
}

// matchNumeric reports whether a and b are the same claim: exact equality
// first, then rounding-tolerance equality via math.Round (e.g. "33%"
// supporting a "33.33%" claim, or vice versa). The tolerance is a fixed
// absolute ±0.5, which is proportionally large at small magnitudes — an
// input "0.5%" would ground an output claim of "1%". Accepted for v1 in the
// same direction as the other documented gaps (errs toward missing a
// fabrication rather than flagging a legitimate rounding), but a caller
// checking small percentages should be aware exact/near matches are not
// distinguished from a materially different value below 1.
func matchNumeric(a, b float64) bool {
	if math.Abs(a-b) < 1e-6 {
		return true
	}
	return math.Round(a) == math.Round(b)
}

// support is the set of numeric values found in the input, used to check
// whether an output claim is grounded.
type support struct {
	numbers []float64
	ratios  [][2]float64
}

// buildSupport extracts every numeric value mentioned anywhere in the input
// (regardless of kind — a year, list index, or bare count in the input can
// still ground an eligible output claim) plus every "N out of M" ratio, so
// isSupported can also check the derived N/M*100 percentage.
func buildSupport(input string) support {
	var sup support
	seen := map[float64]bool{}
	add := func(n float64) {
		if !seen[n] {
			seen[n] = true
			sup.numbers = append(sup.numbers, n)
		}
	}
	for _, c := range extractClaims(input) {
		add(c.num)
		if c.num2 != nil {
			add(*c.num2)
			sup.ratios = append(sup.ratios, [2]float64{c.num, *c.num2})
		}
	}
	return sup
}

// isSupported reports whether c has support in sup — see the "Support" bullet
// list on CheckGrounding for the exact rules per kind.
func isSupported(c rawClaim, sup support) bool {
	switch c.kind {
	case ClaimRatio:
		for _, r := range sup.ratios {
			if matchNumeric(r[0], c.num) && c.num2 != nil && matchNumeric(r[1], *c.num2) {
				return true
			}
		}
		if c.num2 != nil && *c.num2 != 0 {
			derived := c.num / *c.num2 * 100
			for _, n := range sup.numbers {
				if matchNumeric(n, derived) {
					return true
				}
			}
		}
		return false
	case ClaimPercent:
		for _, n := range sup.numbers {
			if matchNumeric(n, c.num) {
				return true
			}
		}
		for _, r := range sup.ratios {
			if r[1] != 0 && matchNumeric(r[0]/r[1]*100, c.num) {
				return true
			}
		}
		return false
	default: // currency, count
		for _, n := range sup.numbers {
			if matchNumeric(n, c.num) {
				return true
			}
		}
		return false
	}
}

// CheckGrounding is the guardrail's entry point. It extracts numeric/
// statistic claims from output and checks each ELIGIBLE claim (percent,
// ratio, currency, count — see ClaimKind) against input, deterministically
// and locally: no LLM judge, no network call.
//
// Support (a claim counts as grounded if ANY of these hold):
//   - The exact same numeric value appears anywhere in input (tolerant of
//     rounding — "33%" supports a "33.33%" claim and vice versa).
//   - For a percent claim: input states an "N out of M" ratio whose derived
//     N/M*100 matches.
//   - For a ratio claim: input states the identical "N out of M" ratio, or
//     input states the derived percentage directly.
//
// v1 deliberately has NO semantic derivation (no "majority" implies ">50%",
// no word-to-number mapping). That is what makes the motivating incident
// work: input phrases like "a majority" / "a small fraction" carry no
// number at all, so any percent the model states in their place has zero
// support and is flagged — exactly the failure this guardrail exists to
// catch. Do not add semantic mappings here; see the package doc comment.
//
// Ineligible kinds (year, list index/ordinal) are extracted but never
// checked or flagged — they are structural or calendar tokens, not
// statistics being asserted.
//
// A zero-claim output (no eligible numeric claims at all) is
// Grounded==true, Score==1.0, Flagged==nil — see GroundingResult.Grounded's
// doc comment for why that must not be read as "this output is true".
func CheckGrounding(input, output string) GroundingResult {
	sup := buildSupport(input)
	claims := extractClaims(output)

	var flagged []Claim
	eligibleCount, groundedCount := 0, 0
	for _, c := range claims {
		if !eligible(c.kind) {
			continue
		}
		eligibleCount++
		if isSupported(c, sup) {
			groundedCount++
			continue
		}
		flagged = append(flagged, Claim{
			Value:  c.text,
			Kind:   c.kind,
			Reason: fmt.Sprintf("%q not found in, or derivable from, the input", c.text),
		})
	}

	score := 1.0
	if eligibleCount > 0 {
		score = float64(groundedCount) / float64(eligibleCount)
	}
	return GroundingResult{
		Grounded:      len(flagged) == 0,
		Score:         score,
		ClaimsChecked: eligibleCount,
		Flagged:       flagged,
	}
}
