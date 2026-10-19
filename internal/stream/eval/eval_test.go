package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/stream/event"
	"github.com/aceteam-ai/citadel-cli/internal/stream/rules"
)

func writeTestFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func testPack(t testing.TB) string {
	t.Helper()
	path := t.TempDir()
	writeTestFile(t, filepath.Join(path, "pack.yml"), "id: eval-test\nversion: 1.0.0\nparser: json\nfield_map:\n  text: message\n  source: source.ip\n  stamp: '@timestamp'\nrules:\n  - id: detect\n    version: 1\n    file: detect.yml\n")
	writeTestFile(t, filepath.Join(path, "detect.yml"), "title: Deterministic offline test\nname: detect\ndetection:\n  selected:\n    message|contains: alert\n  condition: selected\nx-aceteam:\n  group_by: [source.ip]\n  escalate: false\n")
	return path
}

func logLine(text, stamp string) string {
	b, _ := json.Marshal(map[string]string{"text": text, "source": "192.0.2.1", "stamp": stamp})
	return string(b) + "\n"
}

func TestRunSortsScenariosResetsStateAndHashesCanonicalReport(t *testing.T) {
	pack := testPack(t)
	logs := t.TempDir()
	writeTestFile(t, filepath.Join(logs, "z.log"), logLine("alert secret-evidence", "2026-10-19T10:00:00Z"))
	writeTestFile(t, filepath.Join(logs, "a.jsonl"), logLine("benign", "2026-10-19T10:00:00Z"))
	writeTestFile(t, filepath.Join(logs, "ignored.txt"), "not an input")
	options := Options{AllowedLateness: 30 * time.Second}
	first, err := Run(context.Background(), pack, logs, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), pack, logs, options)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("identical inputs did not produce identical reports")
	}
	if first.TimeMode != "ingest" || len(first.Scenarios) != 2 || first.Scenarios[0].ID != "a" || first.Scenarios[1].ID != "z" || len(first.Scenarios[0].Matches) != 0 || len(first.Scenarios[1].Matches) != 1 {
		t.Fatalf("incorrect scenario report: %+v", first)
	}
	match := first.Scenarios[1].Matches[0]
	if match.TMonoNS != int64(time.Millisecond) || match.TWallNS != ReplayEpochNS+int64(time.Millisecond) || !reflect.DeepEqual(match.RawRefs, []string{"z.log:0"}) {
		t.Fatalf("replay envelope or source reference changed: %+v", match)
	}
	gotDigest := first.OutputDigest
	first.OutputDigest = ""
	canonical, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	if gotDigest != "sha256:"+hex.EncodeToString(digest[:]) {
		t.Fatal("output digest is not canonical report SHA-256")
	}
	if strings.Contains(string(canonical), "secret-evidence") || strings.Contains(string(canonical), logs) || strings.Contains(string(canonical), pack) {
		t.Fatal("report leaked raw evidence or absolute input paths")
	}
}

func TestRunEventTimeFlushAndLateStats(t *testing.T) {
	pack := testPack(t)
	path := filepath.Join(t.TempDir(), "event.log")
	writeTestFile(t, path, logLine("alert first", "2026-10-19T10:00:59Z")+logLine("alert second", "2026-10-19T10:00:58Z"))
	report, err := Run(context.Background(), pack, path, Options{TimeMode: "event", AllowedLateness: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Scenarios[0].Matches) != 2 || report.Scenarios[0].Matches[0].WindowEndNS >= report.Scenarios[0].Matches[1].WindowEndNS || report.Scenarios[0].Stats.Late != 0 {
		t.Fatalf("event-time buffer did not flush in order: %+v", report)
	}
	writeTestFile(t, path, logLine("alert first", "2026-10-19T10:01:00Z")+logLine("alert late", "2026-10-19T10:00:01Z"))
	report, err = Run(context.Background(), pack, path, Options{TimeMode: "event", AllowedLateness: 30 * time.Second})
	if err != nil || report.Scenarios[0].Stats.Late != 1 || report.Scenarios[0].Events != 2 || len(report.Scenarios[0].Matches) != 1 {
		t.Fatalf("late event was not counted: %+v, %v", report, err)
	}
}

func TestRunNeverExposesRawGroupingValues(t *testing.T) {
	pack := testPack(t)
	writeTestFile(t, filepath.Join(pack, "detect.yml"), "title: Private grouping test\nname: detect\ndetection:\n  selected:\n    message|contains: alert\n  condition: selected\nx-aceteam:\n  group_by: [message]\n  escalate: false\n")
	path := filepath.Join(t.TempDir(), "private.log")
	writeTestFile(t, path, logLine("alert private-group-secret", "2026-10-19T10:00:00Z"))
	report, err := Run(context.Background(), pack, path, Options{})
	if err != nil || len(report.Scenarios[0].Matches) != 1 {
		t.Fatalf("private grouping evaluation failed: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil || strings.Contains(string(data), "private-group-secret") || !validKeyDigest(report.Scenarios[0].Matches[0].GroupKey) {
		t.Fatal("report exposed raw grouping values instead of the opaque key digest")
	}
}

func TestRunNamesUnsupportedModifierWithoutExposingPaths(t *testing.T) {
	pack := testPack(t)
	writeTestFile(t, filepath.Join(pack, "detect.yml"), "title: Unsupported modifier\nname: detect\ndetection:\n  selected:\n    message|windash: alert\n  condition: selected\n")
	path := filepath.Join(t.TempDir(), "case.log")
	writeTestFile(t, path, logLine("alert", "2026-10-19T10:00:00Z"))
	_, err := Run(context.Background(), pack, path, Options{})
	if err == nil || !strings.Contains(err.Error(), "windash") || !strings.Contains(err.Error(), "detect") || strings.Contains(err.Error(), pack) {
		t.Fatalf("compiler refusal must name its rule/feature without filesystem paths: %v", err)
	}
}

func TestRunRejectsLineByteFileAndAlertLimits(t *testing.T) {
	pack := testPack(t)
	path := filepath.Join(t.TempDir(), "oversize.log")
	writeTestFile(t, path, strings.Repeat("x", event.DefaultMaxLineBytes+1))
	if _, err := Run(context.Background(), pack, path, Options{}); err == nil {
		t.Fatal("oversize line accepted")
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxTotalBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	if _, err := Run(context.Background(), pack, path, Options{}); err == nil {
		t.Fatal("total byte limit accepted")
	}
	logs := t.TempDir()
	for i := 0; i <= MaxFiles; i++ {
		writeTestFile(t, filepath.Join(logs, fmt.Sprintf("source-%03d.log", i)), "")
	}
	if _, err := Run(context.Background(), pack, logs, Options{}); err == nil {
		t.Fatal("file limit accepted")
	}
	scenario := Scenario{Matches: []rules.Alert{}}
	count := MaxAlerts
	if err := appendAlerts(&scenario, []rules.Alert{{RuleID: "detect"}}, &count); err == nil || count != MaxAlerts || len(scenario.Matches) != 0 {
		t.Fatal("alert limit did not refuse before retaining another alert")
	}
}

func TestRunRejectsSymlinksAndDoesNotTraverseDirectories(t *testing.T) {
	pack := testPack(t)
	outside := t.TempDir()
	writeTestFile(t, filepath.Join(outside, "outside.log"), logLine("alert", "2026-10-19T10:00:00Z"))
	logs := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(logs, "linked")); err != nil {
		t.Skip("symlink creation is unavailable on this platform")
	}
	if _, err := Run(context.Background(), pack, filepath.Join(logs, "linked"), Options{}); err == nil {
		t.Fatal("selected directory symlink was followed")
	}
	writeTestFile(t, filepath.Join(logs, "local.log"), logLine("benign", "2026-10-19T10:00:00Z"))
	report, err := Run(context.Background(), pack, logs, Options{})
	if err != nil || len(report.Scenarios) != 1 || len(report.Scenarios[0].Matches) != 0 {
		t.Fatal("directory discovery followed a nested symlink")
	}
	if err := os.Symlink(filepath.Join(outside, "outside.log"), filepath.Join(logs, "linked.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), pack, logs, Options{}); err == nil {
		t.Fatal("selected source symlink was followed")
	}
}

func TestRunRejectsUnparseableInputAndHonorsCancellation(t *testing.T) {
	pack := testPack(t)
	path := filepath.Join(t.TempDir(), "private.log")
	writeTestFile(t, path, "secret-evidence\n")
	if _, err := Run(context.Background(), pack, path, Options{}); err == nil || strings.Contains(err.Error(), "secret-evidence") || strings.Contains(err.Error(), path) {
		t.Fatal("parse error was ignored or leaked evidence/path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, pack, path, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	for _, options := range []Options{{TimeMode: "unknown"}, {AllowedLateness: -time.Second}, {AllowedLateness: 2 * time.Hour}} {
		if _, err := Run(context.Background(), pack, path, options); err == nil {
			t.Fatal("invalid time policy accepted")
		}
	}
}

func TestExpectedComparisonCountsUniqueIdentitiesAndUndefinedMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expected.json")
	keyHash := sha256.Sum256([]byte("[]"))
	key := "sha256:" + hex.EncodeToString(keyHash[:])
	labels, err := json.Marshal(Expected{Scenarios: map[string][]ExpectedMatch{"case": {{RuleID: "detect", GroupKey: key}, {RuleID: "missing", GroupKey: key}}}})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, string(labels))
	scenarios := []Scenario{{ID: "case", Matches: []rules.Alert{{RuleID: "detect", GroupKey: key}, {RuleID: "detect", GroupKey: key}, {RuleID: "unexpected", GroupKey: key}}}}
	got, err := compareExpected(path, scenarios)
	if err != nil || got.TruePositives != 1 || got.FalsePositives != 1 || got.FalseNegatives != 1 || got.Passed || got.Recall == nil || *got.Recall != 0.5 || got.FalseDiscoveryRate == nil || *got.FalseDiscoveryRate != 0.5 {
		t.Fatalf("wrong identity comparison: %+v, %v", got, err)
	}
	writeTestFile(t, path, `{"scenarios":{"case":[]}}`)
	got, err = compareExpected(path, []Scenario{{ID: "case", Matches: []rules.Alert{}}})
	if err != nil || !got.Passed || got.Recall != nil || got.Precision != nil || got.FalseDiscoveryRate != nil {
		t.Fatalf("undefined metrics must stay null: %+v, %v", got, err)
	}
}

func TestExpectedRejectsMissingDuplicateAndUnknownLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expected.json")
	cases := []string{
		`{"scenarios":{"case":[],"case":[]}}`,
		`{"scenarios":{"wrong":[]}}`,
		`{"scenarios":{"case":null}}`,
		`{"scenarios":{"case":[]},"unknown":1}`,
		`{"scenarios":{"case":[{"rule_id":"detect","group_key":"[]","unknown":true}]}}`,
		`{"scenarios":{"case":[{"rule_id":"detect","group_key":"[]"},{"rule_id":"detect","group_key":"[]"}]}}`,
		`{"scenarios":{"case":[]}} {}`,
	}
	for _, content := range cases {
		writeTestFile(t, path, content)
		if _, err := compareExpected(path, []Scenario{{ID: "case"}}); err == nil {
			t.Fatalf("invalid expected labels accepted: %s", content)
		}
	}
}

func TestExpectedRejectsCaseAliasesInsteadOfOverwritingLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(),"expected.json")
	hash := sha256.Sum256([]byte("[]"))
	key := "sha256:"+hex.EncodeToString(hash[:])
	identity := `{"rule_id":"detect","group_key":"`+key+`"}`
	for _, content := range []string{
		`{"Scenarios":{"case":[]}}`,
		`{"scenarios":{"case":[`+identity+`]},"Scenarios":{"case":[]}}`,
		`{"Scenarios":{"case":[]},"scenarios":{"case":[`+identity+`]}}`,
		`{"scenarios":{"case":[{"rule_id":"must-fire","RULE_ID":"detect","group_key":"`+key+`"}]}}`,
		`{"scenarios":{"case":[{"RULE_ID":"detect","group_key":"`+key+`"}]}}`,
		`{"scenarios":{"case":[{"rule_id":"detect","GROUP_KEY":"`+key+`"}]}}`,
		`{"scenarios":{"case":[{"rule_id":"detect","group_key":"`+key+`","unknown":true}]}}`,
		`{"scenarios":{"case":[]},"unknown":true}`,
		`{"scenarios":{"case":[{"rule_id":"detect","group_key":null}]}}`,
	} {
		writeTestFile(t,path,content)
		for _, scenario := range []Scenario{{ID:"case",Matches:[]rules.Alert{}},{ID:"case",Matches:[]rules.Alert{{RuleID:"detect",GroupKey:key}}}} {
			if comparison, err := compareExpected(path,[]Scenario{scenario}); err == nil { t.Fatalf("case-aliased or unknown schema accepted: %s comparison=%+v",content,comparison) }
		}
	}
}

// Throughput and allocations are intentionally reported by the benchmark
// harness, not inserted into a deterministic evaluation report. This simple
// one-filter baseline does not establish the full Shield-pack performance gate.
func BenchmarkOfflineDriverJSON10000(b *testing.B) {
	pack := testPack(b)
	path := filepath.Join(b.TempDir(), "benchmark.log")
	input := strings.Repeat(logLine("benign", "2026-10-19T10:00:00Z"), 10000)
	writeTestFile(b, path, input)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		report, err := Run(context.Background(), pack, path, Options{})
		if err != nil || len(report.Scenarios) != 1 || report.Scenarios[0].Events != 10000 {
			b.Fatalf("offline benchmark failed: %v", err)
		}
	}
}
