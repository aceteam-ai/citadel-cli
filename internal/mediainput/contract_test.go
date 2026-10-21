package mediainput

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

const fixtureSHA256 = "6d19921e8d842ecd00dfba709ad073eacc0511dce7f12e9c2107b0e0f8fcca1e"

type validVector struct {
	Name  string          `json:"name"`
	Task  string          `json:"task"`
	Value json.RawMessage `json:"value"`
}

type invalidVector struct {
	Name    string          `json:"name"`
	Base    string          `json:"base"`
	Task    string          `json:"task"`
	Replace []string        `json:"replace_first"`
	Value   json.RawMessage `json:"value"`
	Reason  string          `json:"reason"`
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
		if _, duplicate := byName[vector.Name]; duplicate {
			t.Fatal("duplicate valid fixture name")
		}
		byName[vector.Name] = vector
		t.Run(vector.Name, func(t *testing.T) {
			contract, err := Parse(vector.Task, vector.Value)
			if err != nil || len(contract.Inputs()) == 0 {
				t.Fatalf("valid vector refused: %v", err)
			}
			assertAcceptedContract(t, vector.Task, contract)
		})
	}
	for _, vector := range invalid {
		t.Run(vector.Name, func(t *testing.T) {
			base, ok := byName[vector.Base]
			if !ok {
				t.Fatal("unknown vector base")
			}
			raw := string(base.Value)
			if len(vector.Value) != 0 {
				raw = string(vector.Value)
			}
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
			_, err := Parse(task, []byte(raw))
			var refusal *Refusal
			if vector.Reason == "" || !errors.As(err, &refusal) || refusal.Reason() != vector.Reason {
				t.Fatalf("refusal=%v, want static reason %q", err, vector.Reason)
			}
		})
	}
}

func namedVector(t *testing.T, name string) validVector {
	t.Helper()
	valid, _ := loadVectors(t)
	for _, vector := range valid {
		if vector.Name == name {
			return vector
		}
	}
	t.Fatalf("missing named fixture %q", name)
	return validVector{}
}

func TestContractCannotBeMutatedThroughAccessor(t *testing.T) {
	vector := namedVector(t, "workspace")
	contract, err := Parse(vector.Task, vector.Value)
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

func TestCapabilityDiagnosticsAlwaysRedact(t *testing.T) {
	inbox := namedVector(t, "inbox")
	contract, err := Parse(inbox.Task, inbox.Value)
	if err != nil {
		t.Fatal(err)
	}
	inputs := contract.Inputs()
	capability := inputs[0].Capability
	secret := capability.Reveal()
	if secret != "synthetic.media-input.source" {
		t.Fatal("explicit reveal changed the token")
	}
	for name, value := range map[string]any{
		"contract":           contract,
		"inputs":             inputs,
		"descriptor":         inputs[0],
		"capability":         capability,
		"private contract":   struct{ contract Contract }{contract},
		"private inputs":     struct{ inputs []Descriptor }{inputs},
		"private descriptor": struct{ descriptor Descriptor }{inputs[0]},
		"private capability": struct{ capability Capability }{capability},
		"private interface":  struct{ value any }{inputs[0]},
	} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%f", "%e", "%p", "%T"} {
			if strings.Contains(fmt.Sprintf(format, value), secret) {
				t.Fatalf("%s leaked via %s", name, format)
			}
		}
		raw, err := json.Marshal(value)
		if err != nil || bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%s leaked or failed JSON serialization", name)
		}
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logger.Info("owned synthetic diagnostic", "capability", capability, "inputs", inputs, "contract", contract)
	if strings.Contains(output.String(), secret) || strings.Contains(capability.String(), secret) || strings.Contains(capability.GoString(), secret) || strings.Contains(capability.LogValue().String(), secret) {
		t.Fatal("credential leaked through logging or direct formatting")
	}
	output.Reset()
	slog.New(slog.NewTextHandler(&output, nil)).Info("owned synthetic diagnostic", "capability", capability, "inputs", inputs, "private", struct{ descriptor Descriptor }{inputs[0]})
	if strings.Contains(output.String(), secret) {
		t.Fatal("credential leaked through text logging")
	}
	if (Capability{}).Reveal() != "" {
		t.Fatal("zero capability must reveal nothing")
	}
}

func TestContractBoundsAndOrdering(t *testing.T) {
	workspace := namedVector(t, "workspace")
	inbox := namedVector(t, "inbox")
	inpaintVector := namedVector(t, "inpaint")
	for _, raw := range [][]byte{
		nil, []byte("null"), []byte("[]"), []byte(`{"schema":null,"inputs":[]}`),
		append(append([]byte(nil), workspace.Value...), []byte(` {}`)...),
		bytes.Repeat([]byte(" "), MaxEnvelopeBytes+1),
		bytes.Replace(workspace.Value, []byte("fixtures/source.png"), []byte{0xff}, 1),
		bytes.Replace(inbox.Value, []byte("synthetic.media-input.source"), bytes.Repeat([]byte("a"), MaxTokenBytes+1), 1),
		bytes.Replace(workspace.Value, []byte(`"size":1`), []byte(`"size":9223372036854775808`), 1),
	} {
		if _, err := Parse(TaskImageToImage, raw); err == nil {
			t.Errorf("invalid raw contract accepted (%d bytes)", len(raw))
		}
	}
	var inpaint map[string]json.RawMessage
	if err := json.Unmarshal(inpaintVector.Value, &inpaint); err != nil {
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
		assertAcceptedContract(t, task, contract)
	})
}

func assertAcceptedContract(t *testing.T, task string, contract Contract) {
	t.Helper()
	want := 1
	if task == TaskInpaint {
		want = 2
	} else if task != TaskImageToImage {
		t.Fatal("accepted unsupported task")
	}
	inputs := contract.Inputs()
	if len(inputs) != want {
		t.Fatal("accepted invalid cardinality")
	}
	for i, input := range inputs {
		role := "source"
		if i == 1 {
			role = "mask"
		}
		if input.Role != role || input.Size < 1 || input.Size > MaxFileBytes {
			t.Fatal("accepted invalid role/size")
		}
		if len(input.SHA256) != 64 || input.SHA256 != strings.ToLower(input.SHA256) {
			t.Fatal("accepted noncanonical hash")
		}
		if _, err := hex.DecodeString(input.SHA256); err != nil {
			t.Fatal("accepted nonhex hash")
		}
		if input.Format != "png" && (role == "mask" || input.Format != "jpeg" && input.Format != "webp") {
			t.Fatal("accepted invalid image format")
		}
		switch input.Origin {
		case "workspace":
			if !ValidPath(input.Path) || input.Capability.Reveal() != "" {
				t.Fatal("accepted unsafe workspace location")
			}
		case "inbox":
			if input.Path != "" || !validCapability(input.Capability.Reveal()) {
				t.Fatal("accepted unsafe capability location")
			}
		default:
			t.Fatal("accepted unknown origin")
		}
	}
}

func TestReadValueHasExactlyFourLevels(t *testing.T) {
	for _, test := range []struct {
		raw string
		ok  bool
	}{
		{`[[["leaf"]]]`, true},
		{`[[[["leaf"]]]]`, false},
	} {
		d := json.NewDecoder(strings.NewReader(test.raw))
		budget := 128
		_, err := readValue(d, 0, &budget)
		if (err == nil) != test.ok {
			t.Fatalf("four-level bound: %v, want accepted=%v", err, test.ok)
		}
	}
}

func TestExportedPathRuleIsSlashOnlyAndByteBounded(t *testing.T) {
	for _, name := range []string{"NUL", "con.png", "dir/ordinary file.png", "dir/" + strings.Repeat("a", 255), "dir/" + strings.Repeat("é", 127) + "a"} {
		if !ValidPath(name) {
			t.Fatal("portable lexical path refused")
		}
	}
	for _, name := range []string{"", ".", "..", "../a", "/a", "a//b", "a/./b", "a/../b", "a/", "a\\b", "C:/a", "a\x00b", "a\u00a0b", "a\uff0eb", "a\u200bb", "a\u202eb", "a\u2028b", "a\u2029b", "a\ufeffb", "cafe\u0301.png", string([]byte{0xff}), strings.Repeat("a", 256), strings.Repeat("é", 128)} {
		if ValidPath(name) {
			t.Fatal("unsafe lexical path accepted")
		}
	}
}
