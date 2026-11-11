package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	streameval "github.com/aceteam-ai/citadel-cli/internal/stream/eval"
)

func streamCommandFixture(t *testing.T) (string, string) {
	t.Helper()
	path := t.TempDir()
	files := map[string]string{
		"pack.yml":   "id: command-test\nversion: 1.0.0\nparser: json\nfield_map:\n  text: message\nrules:\n  - id: detect\n    version: 1\n    file: detect.yml\n",
		"detect.yml": "title: Offline command test\nname: detect\ndetection:\n  selected:\n    message|contains: alert\n  condition: selected\nx-aceteam:\n  escalate: false\n",
		"case.log":   "{\"text\":\"alert secret-evidence\"}\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return path, filepath.Join(path, "case.log")
}

func TestStreamEvalCommandWritesDeterministicJSON(t *testing.T) {
	pack, logs := streamCommandFixture(t)
	var previous string
	for i := 0; i < 2; i++ {
		command := newStreamCommand()
		var output, diagnostic bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&diagnostic)
		command.SetArgs([]string{"eval", pack, logs})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		var report streameval.Report
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report.TimeMode != "ingest" || report.AllowedLatenessNS != 30000000000 || len(report.Scenarios) != 1 || len(report.Scenarios[0].Matches) != 1 || report.OutputDigest == "" {
			t.Fatalf("incorrect command output: %+v", report)
		}
		if strings.Contains(output.String(), "secret-evidence") || strings.Contains(output.String(), pack) || diagnostic.Len() != 0 {
			t.Fatal("command leaked raw logs, paths, or diagnostic output")
		}
		if i != 0 && output.String() != previous {
			t.Fatal("command output changed between identical evaluations")
		}
		previous = output.String()
	}
}

func TestStreamEvalCommandExpectationMismatchReturnsReportAndError(t *testing.T) {
	pack, logs := streamCommandFixture(t)
	expected := filepath.Join(t.TempDir(), "expected.json")
	if err := os.WriteFile(expected, []byte(`{"scenarios":{"case":[]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	command := newStreamCommand()
	var output, diagnostic bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&diagnostic)
	command.SetArgs([]string{"eval", pack, logs, "--expected", expected})
	if err := command.Execute(); err == nil {
		t.Fatal("expectation mismatch did not produce a failing exit")
	}
	var report streameval.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.Comparison == nil || report.Comparison.FalsePositives != 1 || report.Comparison.Passed {
		t.Fatalf("mismatch did not preserve machine-readable report: %+v, %v", report, err)
	}
}

func TestStreamEvalCommandRefusesBadArgumentsAndTimePolicy(t *testing.T) {
	pack, logs := streamCommandFixture(t)
	for _, arguments := range [][]string{{"eval"}, {"eval", pack, logs, "--time", "unrecognized"}, {"eval", pack, logs, "--allowed-lateness", "-1s"}} {
		command := newStreamCommand()
		var output, diagnostic bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&diagnostic)
		command.SetArgs(arguments)
		if err := command.Execute(); err == nil || output.Len() != 0 {
			t.Fatal("invalid invocation accepted or emitted a partial report")
		}
	}
}
