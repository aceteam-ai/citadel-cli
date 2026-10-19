package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type testRuleFile struct {
	id string
	body string
	optional bool
}

func writeTestPack(t *testing.T, files ...testRuleFile) string {
	t.Helper()
	dir := t.TempDir()
	manifest := Manifest{ID: "synthetic", Version: "1.0.0", Parser: "json"}
	for _, file := range files {
		name := file.id+".yml"
		manifest.Rules = append(manifest.Rules, RuleEntry{ID: file.id, Version: "1", File: name, Optional: file.optional})
		if err := os.WriteFile(filepath.Join(dir, name), []byte(file.body), 0600); err != nil { t.Fatal(err) }
	}
	data, err := yaml.Marshal(manifest)
	if err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(dir, "pack.yml"), data, 0600); err != nil { t.Fatal(err) }
	return dir
}

func loadTestPack(t *testing.T, files ...testRuleFile) *Pack {
	t.Helper()
	p, err := LoadPack(writeTestPack(t, files...))
	if err != nil { t.Fatal(err) }
	return p
}

func testDetection(action string) string {
	return "title: Synthetic test\ndetection:\n  selection:\n    event.action: "+action+"\n  condition: selection\n"
}

func testCorrelation(kind, refs, groups, condition string) string {
	body := "title: Synthetic correlation\ncorrelation:\n  type: "+kind+"\n  rules: ["+refs+"]\n  timespan: 10s\n"
	if groups != "" { body += "  group-by: ["+groups+"]\n" }
	if condition != "" { body += "  condition:\n"+condition }
	return body
}

func TestPackOptionalUnsupportedModifierIsVisibleAndMandatoryIsRefused(t *testing.T) {
	unsupported := "title: Unsupported\ndetection:\n  selection:\n    process.name|windash: powershell\n  condition: selection\n"
	valid := testRuleFile{id: "valid", body: testDetection("success")}
	p := loadTestPack(t, valid, testRuleFile{id: "optional", body: unsupported, optional: true})
	if len(p.Refused) != 1 || p.Refused[0].RuleID != "optional" || !strings.Contains(p.Refused[0].Reason, "windash") { t.Fatalf("unsupported modifier refusal not visible: %+v", p.Refused) }
	if len(p.rules) != 1 || p.rules[0].ID != "valid" { t.Fatalf("optional rule half-loaded: %+v", p.rules) }
	_, err := LoadPack(writeTestPack(t, valid, testRuleFile{id: "mandatory", body: unsupported}))
	if err == nil || !strings.Contains(err.Error(), "windash") { t.Fatalf("mandatory unsupported modifier was not refused by name: %v", err) }
}

func TestPackRefusesInvalidCorrelationGraph(t *testing.T) {
	base := testRuleFile{id: "base", body: testDetection("failure")}
	cases := []struct{name, reason string; rules []testRuleFile}{
		{"unknown type", "unsupported correlation type", []testRuleFile{base, {id:"bad", body:testCorrelation("unbounded_join", "base", "", "")}}},
		{"missing reference", "unavailable", []testRuleFile{base, {id:"bad", body:testCorrelation("event_count", "missing", "", "    gte: 1\n")}}},
		{"cycle", "cyclic", []testRuleFile{{id:"a", body:testCorrelation("event_count", "b", "", "    gte: 1\n")}, {id:"b", body:testCorrelation("event_count", "a", "", "    gte: 1\n")}}},
		{"duplicate reference", "duplicate", []testRuleFile{base, {id:"bad", body:testCorrelation("event_count", "base, base", "", "    gte: 1\n")}}},
		{"unsupported threshold", "unsupported correlation condition", []testRuleFile{base, {id:"bad", body:testCorrelation("event_count", "base", "", "    count_forever: 1\n")}}},
		{"missing value field", "field", []testRuleFile{base, {id:"bad", body:testCorrelation("value_count", "base", "", "    gte: 1\n")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadPack(writeTestPack(t, tc.rules...))
			if err == nil || !strings.Contains(err.Error(), tc.reason) { t.Fatalf("expected %q refusal, got %v", tc.reason, err) }
		})
	}
}

func TestPackOptionalUnavailableCorrelationIsRefusedWithoutDiscardingValidRules(t *testing.T) {
	p := loadTestPack(t,
		testRuleFile{id:"valid", body:testDetection("success")},
		testRuleFile{id:"optional", body:testCorrelation("event_count", "unavailable", "", "    gte: 1\n"), optional:true},
	)
	if len(p.rules) != 1 || p.rules[0].ID != "valid" || len(p.Refused) != 1 || p.Refused[0].RuleID != "optional" { t.Fatalf("optional graph refusal was not isolated: rules=%+v refused=%+v", p.rules, p.Refused) }
}

func TestPackDigestIsCanonicalForManifestRuleOrderAndChangesWithRuleBytes(t *testing.T) {
	a := testRuleFile{id:"a", body:testDetection("failure")}
	b := testRuleFile{id:"b", body:testDetection("success")}
	first := loadTestPack(t, a, b)
	second := loadTestPack(t, b, a)
	if first.Digest != second.Digest || !strings.HasPrefix(first.Digest, "sha256:") || len(first.Digest) != 71 { t.Fatalf("rule enumeration changed canonical digest: %s %s", first.Digest, second.Digest) }
	b.body += "description: Different reviewed source bytes\n"
	third := loadTestPack(t, a, b)
	if first.Digest == third.Digest { t.Fatal("rule content mutation did not change pack digest") }
}

func TestPackDigestPinRejectsMismatchedManifest(t *testing.T) {
	dir := writeTestPack(t, testRuleFile{id:"base", body:testDetection("failure")})
	path := filepath.Join(dir, "pack.yml")
	data, err := os.ReadFile(path)
	if err != nil { t.Fatal(err) }
	data = append(data, []byte("digest: sha256:not-the-reviewed-pack\n")...)
	if err := os.WriteFile(path, data, 0600); err != nil { t.Fatal(err) }
	if _, err := LoadPack(dir); err == nil || !strings.Contains(err.Error(), "digest mismatch") { t.Fatalf("mismatched pinned digest accepted: %v", err) }
}

func TestPackRejectsTraversalAliasesDuplicateKeysAndMultipleDocuments(t *testing.T) {
	for _, body := range []string{
		"title: A\ntitle: B\ndetection:\n  a: {event.action: failure}\n  condition: a\n",
		"title: A\ndetection:\n  a: &selection {event.action: failure}\n  b: *selection\n  condition: a\n",
		testDetection("failure")+"---\ntitle: second document\n",
	} {
		if _, err := LoadPack(writeTestPack(t, testRuleFile{id:"bad", body:body})); err == nil { t.Fatalf("unsafe YAML accepted: %q", body) }
	}
	dir := writeTestPack(t, testRuleFile{id:"base", body:testDetection("failure")})
	manifest := "id: synthetic\nversion: 1\nparser: json\nrules:\n  - id: outside\n    version: 1\n    file: ../outside.yml\n"
	if err := os.WriteFile(filepath.Join(dir,"pack.yml"), []byte(manifest),0600); err != nil { t.Fatal(err) }
	if _, err := LoadPack(dir); err == nil { t.Fatal("pack traversal accepted") }
}

func TestPackOptionalRefusalReasonIsDeterministic(t *testing.T) {
	dir := writeTestPack(t,
		testRuleFile{id:"valid", body:testDetection("success")},
		testRuleFile{id:"bad", body:testDetection("failure")+"unsupported_a: true\nunsupported_z: true\n", optional:true},
	)
	var first string
	for i := 0; i < 100; i++ {
		p, err := LoadPack(dir)
		if err != nil { t.Fatal(err) }
		if len(p.Refused) != 1 { t.Fatal("optional refusal missing") }
		if i == 0 { first = p.Refused[0].Reason }
		if p.Refused[0].Reason != first { t.Fatalf("same bytes produced unstable refusal: %q vs %q", first, p.Refused[0].Reason) }
	}
}
