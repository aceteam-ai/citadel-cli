package jobs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestEmbeddingPayloadBounds(t *testing.T) {
	array := func(items []string) string {
		t.Helper()
		raw, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, tc := range []struct {
		name, input string
		wantOK      bool
	}{
		{"scalar at limit", strings.Repeat("a", 64<<10), true},
		{"scalar above limit", strings.Repeat("a", (64<<10)+1), false},
		{"items at limit", array(make([]string, 1024)), true},
		{"items above limit", array(make([]string, 1025)), false},
		{"single array text at limit", array([]string{strings.Repeat("a", 64<<10)}), true},
		{"single array text above limit", array([]string{strings.Repeat("a", (64<<10)+1)}), false},
		{"multibyte byte bound", array([]string{strings.Repeat("é", (64<<10)/2+1)}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEmbeddingPayload(map[string]string{"model": "fixture-model", "input": tc.input})
			if (err == nil) != tc.wantOK {
				t.Fatalf("accepted=%v want=%v", err == nil, tc.wantOK)
			}
			if !tc.wantOK && (err == nil || err.Error() != "embedding input exceeds count or byte limits") {
				t.Fatal("unbounded admission error", err)
			}
		})
	}
	items := make([]string, 64)
	for i := range items {
		items[i] = strings.Repeat("a", 64<<10)
	}
	if _, err := parseEmbeddingPayload(map[string]string{"model": "fixture-model", "input": array(items)}); err != nil {
		t.Fatal("aggregate byte boundary refused", err)
	}
	items = append(items, "a")
	if _, err := parseEmbeddingPayload(map[string]string{"model": "fixture-model", "input": array(items)}); err == nil {
		t.Fatal("aggregate byte bound ignored")
	}
}

func TestEmbeddingDimensionParseErrorDoesNotReflectInput(t *testing.T) {
	for _, value := range []string{"fixture-private-text", strings.Repeat("9", 1000), "3.5"} {
		_, err := parseEmbeddingPayload(map[string]string{"model": "fixture-model", "input": "a", "dimensions": value})
		if err == nil || err.Error() != "dimensions must be an integer" {
			t.Fatal("dimension parse error reflected supplied input", err)
		}
	}
}

func TestEmbeddingInputBoundsKeepScalarCompatibility(t *testing.T) {
	for _, raw := range []string{"plain text", "[unfinished", `[1]`, `["a"] extra`, `"quoted text"`, "   "} {
		req, err := parseEmbeddingPayload(map[string]string{"model": "fixture-model", "input": raw})
		if err != nil || len(req.Input) != 1 || req.Input[0] != raw {
			t.Fatalf("scalar fallback changed: %q %v", raw, err)
		}
	}
	for _, raw := range []string{"null", "[]"} {
		if _, err := parseEmbeddingPayload(map[string]string{"model": "fixture-model", "input": raw}); err == nil {
			t.Fatal("empty array became scalar input")
		}
	}
	// Count decoded UTF-8 bytes, not JSON escape bytes. A maximum-size text
	// containing escaped controls remains a bounded, valid request.
	raw, err := json.Marshal([]string{strings.Repeat("\x00", 64<<10)})
	if err != nil {
		t.Fatal(err)
	}
	if req, err := parseEmbeddingPayload(map[string]string{"model": "fixture-model", "input": string(raw)}); err != nil || len(req.Input[0]) != 64<<10 {
		t.Fatal("escaped byte boundary refused", err)
	}
	if _, err := boundedEmbeddingInputs(strings.Repeat(" ", embeddingMaxEncodedInputBytes+1)); err != errEmbeddingInputLimit {
		t.Fatal("encoded byte ceiling ignored", err)
	}
}

func TestEmbeddingBoundsRefuseBeforeNetworkOrReceipt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("CITADEL_TEI_URL", srv.URL)
	native, err := NewNativeEmbeddingHandler(srv.URL, "fixture-model", 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []*EmbeddingHandler{{}, native} {
		signer := &nativeEmbeddingCountingSigner{inMemSigner: newInMemSigner(t)}
		h.Signer, h.Dispatched = signer, true
		for _, input := range []string{strings.Repeat("a", (64<<10)+1), "[" + strings.Repeat(`"a",`, 1024) + `"a"]`} {
			job := nativeEmbeddingJob("fixture-model", "2")
			job.Payload["input"] = input
			if out, err := h.Execute(JobContext{}, job); err == nil || out != nil {
				t.Fatal("oversized job produced output", err)
			}
		}
		if signer.calls != 0 {
			t.Fatal("rejected input produced a signed receipt")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("rejected input contacted an engine")
	}
}
