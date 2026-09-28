package jobs

import (
	"encoding/json"
	"testing"
)

func TestReviewPythonNumberParity(t *testing.T) {
	for raw, want := range map[string]string{
		`{"minimum":1e0}`:  "056391933b2ef5574a8fe5004b160370752464b2d77a4c34d05c3a38b9a104c0",
		`{"minimum":1.00}`: "056391933b2ef5574a8fe5004b160370752464b2d77a4c34d05c3a38b9a104c0",
		`{"minimum":-0}`:   "58ee87133de42ae5c99f4dc0ff6083f3d0d11a23d15d6f4c77375f6249ea87d8",
		`{"minimum":1E-6}`: "3fad80539e5ac4c2a65ae831863f0b181db8d9b681ac8048acb7a3e4602af30c",
	} {
		got, err := ComputeTemplateManifestHash("k", 1, json.RawMessage(raw), json.RawMessage(`{}`), json.RawMessage(`{"kind":"builtin","handler":"h"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Python parity broken for %s: got %s want %s", raw, got, want)
		}
	}
}
