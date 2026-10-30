package containerjobs

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestContainerPairedManifestFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/wire-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Manifest  Manifest `json:"manifest"`
		Canonical string   `json:"canonical_manifest_json"`
		Digest    string   `json:"manifest_sha256"`
		Types     []string `json:"job_types"`
	}
	if json.Unmarshal(b, &fixture) != nil {
		t.Fatal("fixture")
	}
	raw, _ := json.Marshal(fixture.Manifest)
	if string(raw) != fixture.Canonical || manifestDigest(&fixture.Manifest) != fixture.Digest || !reflect.DeepEqual(Types, fixture.Types) {
		t.Fatal("paired wire contract drift")
	}
}
