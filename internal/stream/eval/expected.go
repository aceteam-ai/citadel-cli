package eval

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/aceteam-ai/citadel-cli/internal/stream/ops"
)

// Expected uses unique (rule_id, group_key) identities per scenario, not event
// count or alert count. GroupKey is the opaque sha256 key emitted by the engine,
// never the raw grouping values. Repeated matches for one identity count once.
// Every evaluated scenario must be labeled, including benign scenarios with [].
type Expected struct {
	Scenarios map[string][]ExpectedMatch `json:"scenarios"`
}

type ExpectedMatch struct {
	RuleID string `json:"rule_id"`
	GroupKey string `json:"group_key"`
}

type Comparison struct {
	Unit string `json:"unit"`
	Scenarios uint64 `json:"scenarios"`
	ExpectedMatches uint64 `json:"expected_matches"`
	ActualMatches uint64 `json:"actual_matches"`
	TruePositives uint64 `json:"true_positives"`
	FalsePositives uint64 `json:"false_positives"`
	FalseNegatives uint64 `json:"false_negatives"`
	Recall *float64 `json:"recall"`
	Precision *float64 `json:"precision"`
	FalseDiscoveryRate *float64 `json:"false_discovery_rate"`
	Passed bool `json:"passed"`
}

func compareExpected(path string, scenarios []Scenario) (*Comparison, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("stream eval expectations must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("stream eval could not open expectations")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("stream eval expectations changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("stream eval expectations exceed the byte limit")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("stream eval expectations must be valid UTF-8")
	}
	// Validate duplicate keys before decoding to maps, where JSON's usual
	// last-key-wins semantics could silently replace a scenario's labels.
	validator := json.NewDecoder(bytes.NewReader(data))
	validator.UseNumber()
	budget := 100000
	if err := validateExpectedJSON(validator, 0, &budget); err != nil {
		return nil, err
	}
	if _, err := validator.Token(); err != io.EOF {
		return nil, errors.New("stream eval expectations contain trailing JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var expected Expected
	if err := decoder.Decode(&expected); err != nil || expected.Scenarios == nil || len(expected.Scenarios) != len(scenarios) {
		return nil, errors.New("stream eval expectations must label exactly the evaluated scenarios")
	}
	comparison := &Comparison{Unit: "unique_rule_and_group_per_scenario", Scenarios: uint64(len(scenarios)), Passed: true}
	var expectedCount int
	for _, scenario := range scenarios {
		labels, exists := expected.Scenarios[scenario.ID]
		if !exists || labels == nil {
			return nil, errors.New("stream eval expectations are missing a scenario label array")
		}
		wanted := make(map[ExpectedMatch]bool)
		for _, label := range labels {
			expectedCount++
			if expectedCount > MaxAlerts || label.RuleID == "" || len(label.RuleID) > 128 || len(label.GroupKey) > ops.MaxKeyBytes || !validKeyDigest(label.GroupKey) || wanted[label] {
				return nil, errors.New("stream eval expectations contain invalid or duplicate match identities")
			}
			wanted[label] = true
		}
		actual := make(map[ExpectedMatch]bool)
		for _, match := range scenario.Matches {
			actual[ExpectedMatch{RuleID: match.RuleID, GroupKey: match.GroupKey}] = true
		}
		comparison.ExpectedMatches += uint64(len(wanted))
		comparison.ActualMatches += uint64(len(actual))
		for identity := range actual {
			if wanted[identity] {
				comparison.TruePositives++
			} else {
				comparison.FalsePositives++
			}
		}
		for identity := range wanted {
			if !actual[identity] {
				comparison.FalseNegatives++
			}
		}
	}
	comparison.Recall = ratio(comparison.TruePositives, comparison.ExpectedMatches)
	comparison.Precision = ratio(comparison.TruePositives, comparison.ActualMatches)
	comparison.FalseDiscoveryRate = ratio(comparison.FalsePositives, comparison.ActualMatches)
	comparison.Passed = comparison.FalsePositives == 0 && comparison.FalseNegatives == 0
	return comparison, nil
}

func validKeyDigest(key string) bool {
	if len(key) != 71 || !strings.HasPrefix(key, "sha256:") || key != strings.ToLower(key) {
		return false
	}
	_, err := hex.DecodeString(key[7:])
	return err == nil
}

func ratio(numerator, denominator uint64) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}

func validateExpectedJSON(decoder *json.Decoder, depth int, budget *int) error {
	if depth > 16 || *budget <= 0 {
		return errors.New("stream eval expectations exceed structural limits")
	}
	*budget -= 1
	token, err := decoder.Token()
	if err != nil {
		return errors.New("stream eval expectations contain malformed JSON")
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, valid := keyToken.(string)
			if err != nil || !valid || keys[key] {
				return errors.New("stream eval expectations contain an invalid or duplicate key")
			}
			keys[key] = true
			if err := validateExpectedJSON(decoder, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("stream eval expectations contain an invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := validateExpectedJSON(decoder, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("stream eval expectations contain an invalid JSON array")
		}
	default:
		return errors.New("stream eval expectations contain an unexpected delimiter")
	}
	return nil
}
