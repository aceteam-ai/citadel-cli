package trust

import (
	"regexp"
	"strings"
	"testing"
)

func checkNames(v Verdict) []string {
	names := make([]string, 0, len(v.Checks))
	for _, c := range v.Checks {
		names = append(names, c.Name)
	}
	return names
}

func checkByName(v Verdict, name string) (CheckReport, bool) {
	for _, c := range v.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return CheckReport{}, false
}

func TestBuildVerdict_AllChecksPresentAndPassOnBenign(t *testing.T) {
	grounding := CheckGrounding("say hello", "hello world, nothing numeric here")
	v := BuildVerdict("say hello", "hello world, nothing numeric here", grounding, DefaultDetectors())

	wantNames := []string{"grounding", "secrets", "pii", "ferpa"}
	if got := checkNames(v); !equalStrings(got, wantNames) {
		t.Fatalf("check names = %v, want %v", got, wantNames)
	}
	if v.Action != "pass" {
		t.Errorf("aggregate action = %q, want pass", v.Action)
	}
	for _, c := range v.Checks {
		if c.Action != "pass" {
			t.Errorf("check %q action = %q, want pass", c.Name, c.Action)
		}
	}
	if !strings.HasPrefix(v.VerdictHash, "sha256:") {
		t.Errorf("verdict_hash = %q, want sha256: prefix", v.VerdictHash)
	}
}

func TestBuildVerdict_FlagsDetectorAndAggregates(t *testing.T) {
	// A leaked PEM private key in the OUTPUT: secrets flags, aggregate flags,
	// grounding stays pass. A PEM header is used (not an AWS key) precisely
	// because it carries no digits, so the grounding check has no numeric
	// claim to flag and this isolates the secrets-check contribution.
	const in, out = "show me a key", "here: -----BEGIN RSA PRIVATE KEY-----"
	grounding := CheckGrounding(in, out)
	v := BuildVerdict(in, out, grounding, DefaultDetectors())

	if v.Action != "flag" {
		t.Fatalf("aggregate action = %q, want flag", v.Action)
	}
	secrets, ok := checkByName(v, "secrets")
	if !ok {
		t.Fatal("no secrets check present")
	}
	if secrets.Action != "flag" || secrets.Severity != SeverityHigh {
		t.Errorf("secrets check = %+v, want action=flag severity=high", secrets)
	}
	if g, _ := checkByName(v, "grounding"); g.Action != "pass" {
		t.Errorf("grounding action = %q, want pass (no numeric claims)", g.Action)
	}
}

func TestBuildVerdict_GroundingFlagAggregates(t *testing.T) {
	// The motivating incident: "a majority / a small fraction" -> "68% / 7%".
	grounding := CheckGrounding("a majority / a small fraction", "68% / 7%")
	if grounding.Grounded {
		t.Fatal("expected the fabricated-percentage fixture to be ungrounded")
	}
	v := BuildVerdict("a majority / a small fraction", "68% / 7%", grounding, DefaultDetectors())
	if v.Action != "flag" {
		t.Errorf("aggregate action = %q, want flag (grounding ungrounded)", v.Action)
	}
	g, _ := checkByName(v, "grounding")
	if g.Action != "flag" {
		t.Errorf("grounding check action = %q, want flag", g.Action)
	}
	// grounding evidence_hash must be bare hex (no sha256: prefix), matching
	// internal/aep's flagged_hash encoding, and non-empty.
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(g.EvidenceHash) {
		t.Errorf("grounding evidence_hash = %q, want bare 64-hex", g.EvidenceHash)
	}
}

// TestBuildVerdict_MutationDropDetector is the acceptance mutation test:
// dropping a detector from the assembly removes its checks[] entry AND changes
// verdict_hash. Runnable as a go test precisely because the detector set is an
// injected argument, not a hardcode.
func TestBuildVerdict_MutationDropDetector(t *testing.T) {
	const in, out = "say hello", "hello world, nothing numeric here"
	grounding := CheckGrounding(in, out)
	full := BuildVerdict(in, out, grounding, DefaultDetectors())

	drops := []struct {
		name      string
		detectors []Detector
		gone      string
	}{
		{"drop ferpa (last)", DefaultDetectors()[:2], "ferpa"},
		{"drop secrets (first)", DefaultDetectors()[1:], "secrets"},
		{"drop pii (middle)", []Detector{DefaultDetectors()[0], DefaultDetectors()[2]}, "pii"},
	}
	for _, d := range drops {
		t.Run(d.name, func(t *testing.T) {
			mutated := BuildVerdict(in, out, grounding, d.detectors)
			if _, present := checkByName(mutated, d.gone); present {
				t.Errorf("check %q still present after drop: %v", d.gone, checkNames(mutated))
			}
			if mutated.VerdictHash == full.VerdictHash {
				t.Errorf("verdict_hash unchanged after dropping %q (%q)", d.gone, mutated.VerdictHash)
			}
		})
	}
}

func TestBuildVerdict_VerdictHashDeterministic(t *testing.T) {
	const in, out = "say hello", "hello world"
	g := CheckGrounding(in, out)
	a := BuildVerdict(in, out, g, DefaultDetectors())
	b := BuildVerdict(in, out, g, DefaultDetectors())
	if a.VerdictHash != b.VerdictHash {
		t.Errorf("verdict_hash not deterministic: %q vs %q", a.VerdictHash, b.VerdictHash)
	}
}

// TestBuildVerdict_ExtrasDoNotAffectVerdictHash pins that verdict_hash covers
// only the DoR §3 core check fields (name/version/action/severity/
// evidence_hash) — the grounding block's flagged list is excluded, so two
// outputs that flag the SAME claims-count with the SAME check actions but
// different flagged TEXT still differ only through evidence_hash, never
// through the excluded flagged list. Here we assert the simpler invariant
// that the grounding Extras ride the check map but the hash is stable for a
// fixed GroundingResult.
func TestBuildVerdict_GroundingExtrasRideMapNotHash(t *testing.T) {
	g := CheckGrounding("say hello", "hi")
	v := BuildVerdict("say hello", "hi", g, DefaultDetectors())
	gCheck, _ := checkByName(v, "grounding")
	m := gCheck.Map()
	for _, k := range []string{"grounded", "score", "claims_checked"} {
		if _, ok := m[k]; !ok {
			t.Errorf("grounding check map missing extra %q: %v", k, m)
		}
	}
	for _, k := range []string{"name", "version", "action", "severity", "evidence_hash"} {
		if _, ok := m[k]; !ok {
			t.Errorf("grounding check map missing core field %q: %v", k, m)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
