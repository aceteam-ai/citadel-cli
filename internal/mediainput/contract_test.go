package mediainput

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

const fixtureSHA256 = "2dc946dcea1cfbdf28f33f5cea1b7df1ad7ab2adecd0dcd875d5173aa31a26d5"

type validVector struct {
	Name  string          `json:"name"`
	Task  string          `json:"task"`
	Value json.RawMessage `json:"value"`
}

type invalidVector struct {
	Name    string   `json:"name"`
	Base    string   `json:"base"`
	Task    string   `json:"task"`
	Replace []string `json:"replace_first"`
}

func loadVectors(t *testing.T) ([]validVector, []invalidVector) {
	t.Helper()
	raw, err := os.ReadFile("testdata/media_input_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != fixtureSHA256 {
		t.Fatal("shared fixture changed without a reviewed pin update")
	}
	var fixture struct {
		Schema    string          `json:"fixture_schema"`
		Synthetic bool            `json:"synthetic"`
		Valid     []validVector   `json:"valid"`
		Invalid   []invalidVector `json:"invalid"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || fixture.Schema != "media_input.contract-tests.v1" || !fixture.Synthetic {
		t.Fatalf("invalid synthetic fixture: %v", err)
	}
	return fixture.Valid, fixture.Invalid
}

func TestSharedInputContractVectors(t *testing.T) {
	valid, invalid := loadVectors(t)
	byName := make(map[string]validVector)
	for _, vector := range valid {
		byName[vector.Name] = vector
		t.Run(vector.Name, func(t *testing.T) {
			contract, err := Parse(vector.Task, vector.Value)
			if err != nil || len(contract.Inputs()) == 0 {
				t.Fatalf("valid vector refused: %v", err)
			}
		})
	}
	for _, vector := range invalid {
		t.Run(vector.Name, func(t *testing.T) {
			base, ok := byName[vector.Base]
			if !ok {
				t.Fatal("unknown vector base")
			}
			raw := string(base.Value)
			if len(vector.Replace) != 0 {
				if len(vector.Replace) != 2 || !strings.Contains(raw, vector.Replace[0]) || vector.Replace[0] == vector.Replace[1] {
					t.Fatal("ineffective fixture mutation")
				}
				raw = strings.Replace(raw, vector.Replace[0], vector.Replace[1], 1)
			}
			task := base.Task
			if vector.Task != "" {
				task = vector.Task
			}
			if _, err := Parse(task, []byte(raw)); err == nil {
				t.Fatal("invalid vector accepted")
			}
		})
	}
}

func TestContractCannotBeMutatedThroughAccessor(t *testing.T) {
	valid, _ := loadVectors(t)
	contract, err := Parse(valid[0].Task, valid[0].Value)
	if err != nil {
		t.Fatal(err)
	}
	inputs := contract.Inputs()
	inputs[0].Path = "changed"
	inputs[0].Role = "mask"
	if got := contract.Inputs()[0]; got.Path == "changed" || got.Role != "source" {
		t.Fatal("caller mutated validated descriptors")
	}
}

func TestContractRefusalNeverReflectsInputs(t *testing.T) {
	raw := `{"schema":"media_input.v1","inputs":[{"role":"source","origin":"inbox","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1,"format":"png","capability":"private token from fixture"}]}`
	_, err := Parse(TaskImageToImage, []byte(raw))
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Reason() != "invalid_capability" || strings.Contains(err.Error(), "private token") {
		t.Fatalf("unsafe refusal: %v", err)
	}
}

func TestContractBoundsAndOrdering(t *testing.T) {
	valid, _ := loadVectors(t)
	for _, raw := range [][]byte{
		nil, []byte("null"), []byte("[]"), []byte(`{"schema":null,"inputs":[]}`),
		append(append([]byte(nil), valid[0].Value...), []byte(` {}`)...),
		bytes.Repeat([]byte(" "), MaxEnvelopeBytes+1),
		bytes.Replace(valid[0].Value, []byte("fixtures/source.png"), []byte{0xff}, 1),
		bytes.Replace(valid[1].Value, []byte("synthetic.media-input.source"), bytes.Repeat([]byte("a"), MaxTokenBytes+1), 1),
		bytes.Replace(valid[0].Value, []byte(`"size":1`), []byte(`"size":9223372036854775808`), 1),
	} {
		if _, err := Parse(TaskImageToImage, raw); err == nil {
			t.Errorf("invalid raw contract accepted (%d bytes)", len(raw))
		}
	}
	var inpaint map[string]json.RawMessage
	if err := json.Unmarshal(valid[2].Value, &inpaint); err != nil {
		t.Fatal(err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(inpaint["inputs"], &items); err != nil {
		t.Fatal(err)
	}
	items[0], items[1] = items[1], items[0]
	reversed, _ := json.Marshal(items)
	inpaint["inputs"] = reversed
	raw, _ := json.Marshal(inpaint)
	if _, err := Parse(TaskInpaint, raw); err == nil {
		t.Fatal("noncanonical role order accepted")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(TaskImageToImage, []byte(`{"schema":"media_input.v1","inputs":[]}`))
	f.Add(TaskInpaint, []byte(`null`))
	f.Add(TaskImageToImage, []byte(`{"schema":"media_input.v1","inputs":[{"role":"source","origin":"workspace","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1,"format":"png","path":"fixtures/source.png"}]}`))
	f.Fuzz(func(t *testing.T, task string, raw []byte) {
		contract, err := Parse(task, raw)
		if err != nil {
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("untyped refusal: %v", err)
			}
			return
		}
		inputs := contract.Inputs()
		if len(inputs) < 1 || len(inputs) > 2 || inputs[0].Role != "source" {
			t.Fatal("accepted contract has invalid cardinality or ordering")
		}
	})
}
