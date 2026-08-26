package trust

import "testing"

// TestCheckGrounding_FabricatedPercentages_Flagged pins the motivating
// incident: an insight-extraction run on llama3.1:8b turned the source
// abstract's "a majority" / "a small fraction" into fabricated "68%" / "7%".
// Both must be flagged — the input carries no number at all for the model
// to have derived them from.
func TestCheckGrounding_FabricatedPercentages_Flagged(t *testing.T) {
	input := "The study found that a majority of respondents preferred the new design, while a small fraction reported no opinion."
	output := "The study found that 68% of respondents preferred the new design, while 7% reported no opinion."

	result := CheckGrounding(input, output)

	if result.Grounded {
		t.Fatalf("expected Grounded=false, got true (flagged=%v)", result.Flagged)
	}
	if len(result.Flagged) != 2 {
		t.Fatalf("expected 2 flagged claims, got %d: %+v", len(result.Flagged), result.Flagged)
	}
	values := map[string]bool{}
	for _, c := range result.Flagged {
		values[c.Value] = true
		if c.Kind != ClaimPercent {
			t.Errorf("claim %q: expected kind %q, got %q", c.Value, ClaimPercent, c.Kind)
		}
		if c.Reason == "" {
			t.Errorf("claim %q: expected non-empty Reason", c.Value)
		}
	}
	if !values["68%"] {
		t.Errorf("expected \"68%%\" to be flagged, got %+v", result.Flagged)
	}
	if !values["7%"] {
		t.Errorf("expected \"7%%\" to be flagged, got %+v", result.Flagged)
	}
	if result.Score != 0 {
		t.Errorf("expected score 0 (0 of 2 eligible claims grounded), got %v", result.Score)
	}
	if result.ClaimsChecked != 2 {
		t.Errorf("expected ClaimsChecked=2, got %v", result.ClaimsChecked)
	}
}

// TestCheckGrounding_NumberPresentInInput_NotFlagged is the false-positive
// guardrail: a number that appears verbatim in the source must not be
// flagged.
func TestCheckGrounding_NumberPresentInInput_NotFlagged(t *testing.T) {
	input := "Adoption reached 42% among surveyed teams this quarter."
	output := "Adoption reached 42% among surveyed teams this quarter."

	result := CheckGrounding(input, output)

	if !result.Grounded {
		t.Fatalf("expected Grounded=true, got false (flagged=%+v)", result.Flagged)
	}
	if len(result.Flagged) != 0 {
		t.Fatalf("expected no flagged claims, got %+v", result.Flagged)
	}
	if result.Score != 1.0 {
		t.Errorf("expected score 1.0, got %v", result.Score)
	}
}

// TestCheckGrounding_YearAndListIndex_NotFlagged confirms benign/derivable
// numbers (a calendar year, a list index) are excluded from the claim set
// entirely rather than flagged as unsupported.
func TestCheckGrounding_YearAndListIndex_NotFlagged(t *testing.T) {
	input := "The team shipped the following updates:"
	output := "In 2026, the team shipped two updates:\n1. A new dashboard\n2. Faster onboarding"

	result := CheckGrounding(input, output)

	if !result.Grounded {
		t.Fatalf("expected Grounded=true, got false (flagged=%+v)", result.Flagged)
	}
	if len(result.Flagged) != 0 {
		t.Fatalf("expected no flagged claims (year/list-index are ineligible), got %+v", result.Flagged)
	}
}

// TestCheckGrounding_NoClaims_GroundedTrueEmptyFlags pins the "zero eligible
// claims" semantics explicitly: a prose-only output with nothing numeric
// scores a vacuous 1.0, not because it was verified true, but because there
// was nothing for this guardrail to check. See GroundingResult.Grounded's
// doc comment.
func TestCheckGrounding_NoClaims_GroundedTrueEmptyFlags(t *testing.T) {
	input := "The customer was satisfied with the response time and asked for a follow-up call."
	output := "The customer expressed satisfaction and requested a follow-up."

	result := CheckGrounding(input, output)

	if !result.Grounded {
		t.Fatalf("expected Grounded=true for a claim-free output, got false")
	}
	if result.Flagged != nil {
		t.Fatalf("expected Flagged=nil, got %+v", result.Flagged)
	}
	if result.Score != 1.0 {
		t.Errorf("expected Score=1.0 for zero eligible claims, got %v", result.Score)
	}
	if result.ClaimsChecked != 0 {
		t.Errorf("expected ClaimsChecked=0 (vacuous grounding, not verification), got %v", result.ClaimsChecked)
	}
}

// TestCheckGrounding_UnsupportedRatio_Flagged: a fabricated "N out of M"
// claim with nothing resembling it in the input is flagged.
func TestCheckGrounding_UnsupportedRatio_Flagged(t *testing.T) {
	input := "Most participants said they would recommend the product to a friend."
	output := "8 out of 10 participants said they would recommend the product to a friend."

	result := CheckGrounding(input, output)

	if result.Grounded {
		t.Fatalf("expected Grounded=false, got true")
	}
	if len(result.Flagged) != 1 {
		t.Fatalf("expected 1 flagged claim, got %+v", result.Flagged)
	}
	if result.Flagged[0].Kind != ClaimRatio {
		t.Errorf("expected kind %q, got %q", ClaimRatio, result.Flagged[0].Kind)
	}
}

// TestCheckGrounding_RatioSupportsDerivedPercent: a ratio in the input
// grounds the equivalent percentage claim in the output (cheap arithmetic
// derivation, not semantic inference).
func TestCheckGrounding_RatioSupportsDerivedPercent(t *testing.T) {
	input := "7 out of 100 respondents reported an issue."
	output := "7% of respondents reported an issue."

	result := CheckGrounding(input, output)

	if !result.Grounded {
		t.Fatalf("expected Grounded=true (7%% is derivable from 7 out of 100), got false (flagged=%+v)", result.Flagged)
	}
}

// TestCheckGrounding_PercentSupportsRatioClaim is the mirror of the above:
// an input percentage grounds the equivalent output ratio claim.
func TestCheckGrounding_PercentSupportsRatioClaim(t *testing.T) {
	input := "Adoption reached 50%."
	output := "Adoption reached 50 out of 100 sampled teams."

	result := CheckGrounding(input, output)

	if !result.Grounded {
		t.Fatalf("expected Grounded=true, got false (flagged=%+v)", result.Flagged)
	}
}

// TestCheckGrounding_RoundingTolerance: a claim that differs from the input
// only by rounding is treated as supported.
func TestCheckGrounding_RoundingTolerance(t *testing.T) {
	input := "The measured rate was 33.33%."
	output := "The rate was approximately 33%."

	result := CheckGrounding(input, output)

	if !result.Grounded {
		t.Fatalf("expected Grounded=true (33%% rounds from 33.33%%), got false (flagged=%+v)", result.Flagged)
	}
}

// TestCheckGrounding_CurrencyAndCount cover the remaining eligible kinds.
func TestCheckGrounding_CurrencyAndCount(t *testing.T) {
	t.Run("currency supported", func(t *testing.T) {
		input := "The invoice totaled $420."
		output := "The customer was billed $420."
		result := CheckGrounding(input, output)
		if !result.Grounded {
			t.Fatalf("expected Grounded=true, got false (flagged=%+v)", result.Flagged)
		}
	})

	t.Run("currency fabricated", func(t *testing.T) {
		input := "The invoice totaled $420."
		output := "The customer was billed $999."
		result := CheckGrounding(input, output)
		if result.Grounded {
			t.Fatalf("expected Grounded=false, got true")
		}
		if len(result.Flagged) != 1 || result.Flagged[0].Kind != ClaimCurrency {
			t.Fatalf("expected 1 flagged currency claim, got %+v", result.Flagged)
		}
	})

	t.Run("bare count fabricated", func(t *testing.T) {
		input := "Several servers reported elevated latency."
		output := "14 servers reported elevated latency."
		result := CheckGrounding(input, output)
		if result.Grounded {
			t.Fatalf("expected Grounded=false, got true")
		}
		if len(result.Flagged) != 1 || result.Flagged[0].Kind != ClaimCount {
			t.Fatalf("expected 1 flagged count claim, got %+v", result.Flagged)
		}
	})

	t.Run("bare count supported", func(t *testing.T) {
		input := "14 servers reported elevated latency overnight."
		output := "14 servers reported elevated latency."
		result := CheckGrounding(input, output)
		if !result.Grounded {
			t.Fatalf("expected Grounded=true, got false (flagged=%+v)", result.Flagged)
		}
	})
}

// TestCheckGrounding_MixedClaims_PartialScore: one supported and one
// fabricated claim in the same output — only the fabricated one is
// flagged, and Score reflects the ratio of grounded to eligible claims.
func TestCheckGrounding_MixedClaims_PartialScore(t *testing.T) {
	input := "Revenue grew 12% year over year."
	output := "Revenue grew 12% year over year, driven by a 40% increase in signups."

	result := CheckGrounding(input, output)

	if result.Grounded {
		t.Fatalf("expected Grounded=false, got true")
	}
	if len(result.Flagged) != 1 {
		t.Fatalf("expected 1 flagged claim, got %+v", result.Flagged)
	}
	if result.Flagged[0].Value != "40%" {
		t.Errorf("expected \"40%%\" flagged, got %+v", result.Flagged)
	}
	if result.Score != 0.5 {
		t.Errorf("expected score 0.5 (1 of 2 eligible claims grounded), got %v", result.Score)
	}
	if result.ClaimsChecked != 2 {
		t.Errorf("expected ClaimsChecked=2, got %v", result.ClaimsChecked)
	}
}

// TestCheckGrounding_YearPriorityIsAKnownFalseNegative documents a
// deliberate v1 tradeoff rather than asserting desired behavior: yearRe has
// priority over countRe (see extractClaims), so a fabricated large count that
// happens to look like a plausible year ("1,950 respondents", "2000 users")
// classifies as ClaimYear and is skipped rather than flagged. This is a real
// false negative in the direction the guardrail is supposed to err against
// (flag, don't miss) — kept because disambiguating "2000" (a year) from
// "2000" (a fabricated count) needs context this deterministic v1 does not
// have. See CLAUDE.md's grounding-guardrail entry / the PR's Known
// Limitations for the tracked follow-up.
func TestCheckGrounding_YearPriorityIsAKnownFalseNegative(t *testing.T) {
	input := "Several respondents completed the survey."
	output := "1950 respondents completed the survey."

	result := CheckGrounding(input, output)

	if !result.Grounded {
		t.Fatalf("known-limitation test's premise changed: expected this fabricated count to slip through as an unflagged year, got Grounded=false (flagged=%+v) — if year-vs-count disambiguation was added, update this test to assert the (now correct) flag instead", result.Flagged)
	}
}

// TestGroundingResult_Block pins the default policy: PolicyFlag (the zero
// value) never blocks, even when the result is ungrounded. PolicyGate blocks
// only when ungrounded.
func TestGroundingResult_Block(t *testing.T) {
	ungrounded := GroundingResult{Grounded: false, Flagged: []Claim{{Value: "68%", Kind: ClaimPercent}}}
	grounded := GroundingResult{Grounded: true}

	var defaultPolicy Policy // zero value
	if defaultPolicy != PolicyFlag {
		t.Fatalf("expected zero value of Policy to be PolicyFlag")
	}
	if ungrounded.Block(defaultPolicy) {
		t.Errorf("PolicyFlag must never block, even when ungrounded")
	}
	if ungrounded.Block(PolicyGate) != true {
		t.Errorf("PolicyGate must block an ungrounded result")
	}
	if grounded.Block(PolicyGate) {
		t.Errorf("PolicyGate must not block a grounded result")
	}
}
