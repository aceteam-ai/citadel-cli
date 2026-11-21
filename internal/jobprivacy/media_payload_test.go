package jobprivacy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestMediaPayloadOwnsExecutionAndExactOriginalBytes(t *testing.T) {
	const secret = "synthetic-media-capability"
	raw := []byte(" {\"inputs\":{\"token\":\"" + secret + "\"},\"n\":1e+03,\"unicode\":\"\\u0061\"} \n")
	wantRaw := bytes.Clone(raw)
	payload := map[string]any{
		"inputs": map[string]any{"token": secret},
		"n":      json.Number("1e+03"),
		"bytes":  []byte{0, 1, 2},
		"json":   json.RawMessage(`{"original":true}`),
		"array":  []any{int64(7), []string{"first"}},
		"typed":  map[string]string{"key": "value"},
		"nil":    []any(nil),
	}
	data := map[string]any{"payload": string(raw), "rayId": "synthetic-ray"}
	private := CaptureMedia(MediaGenerate, payload, raw, data)
	if private == nil {
		t.Fatal("private capture missing")
	}
	want, err := private.ExecutionPayload()
	if err != nil || !reflect.DeepEqual(want, payload) {
		t.Fatalf("original execution types changed: %v", err)
	}
	// Neither ingestion ownership nor a trusted caller's projection is shared.
	payload["inputs"].(map[string]any)["token"] = "mutated"
	payload["bytes"].([]byte)[0] = 9
	payload["json"].(json.RawMessage)[0] = 'X'
	payload["array"].([]any)[1].([]string)[0] = "mutated"
	payload["typed"].(map[string]string)["key"] = "mutated"
	data["payload"] = "mutated"
	raw[0] = 'X'
	got, err := private.Clone().ExecutionPayload()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("capture aliased original: %v", err)
	}
	got["inputs"].(map[string]any)["token"] = "projection mutation"
	got["bytes"].([]byte)[0] = 8
	again, _ := private.ExecutionPayload()
	if !reflect.DeepEqual(again, want) {
		t.Fatal("execution projection aliased stored data")
	}
	if got := private.OriginalPayload(); !bytes.Equal(got, wantRaw) {
		t.Fatal("exact number/Unicode/whitespace bytes changed")
	} else {
		got[0] = 'X'
	}
	if !bytes.Equal(private.OriginalPayload(), wantRaw) {
		t.Fatal("raw projection aliased stored bytes")
	}
	gotData, err := private.OriginalData()
	if err != nil || gotData["payload"] != string(wantRaw) {
		t.Fatal("original source data changed")
	}
	gotData["payload"] = "mutated"
	againData, _ := private.OriginalData()
	if againData["payload"] != string(wantRaw) {
		t.Fatal("source projection aliased stored data")
	}
	assertOpaqueMedia(t, private, secret)
}

func assertOpaqueMedia(t *testing.T, private *MediaPayload, secret string) {
	t.Helper()
	for _, value := range []any{
		private, *private, struct{ private *MediaPayload }{private},
		struct{ private MediaPayload }{*private}, []any{private},
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%p"} {
			if strings.Contains(fmt.Sprintf(verb, value), secret) {
				t.Fatalf("private payload leaked through %s", verb)
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil || bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("private JSON leaked or failed: %v", err)
		}
	}
}

func TestMediaPayloadRawOnlyNeverBecomesExecution(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"inputs":{"token":"synthetic-raw-token"}}`), []byte(`{"inputs":`)} {
		private := CaptureMedia(MediaGenerate, nil, raw, nil)
		if private == nil {
			t.Fatal("raw-only capture missing")
		}
		if _, err := private.ExecutionPayload(); !errors.Is(err, ErrMediaProjection) {
			t.Fatal("raw-only capture became executable")
		}
		if !bytes.Equal(private.OriginalPayload(), raw) {
			t.Fatal("raw-only bytes changed")
		}
		assertOpaqueMedia(t, private, "synthetic-raw-token")
	}
	for _, raw := range [][]byte{nil, {}} {
		private := CaptureMedia(MediaGenerate, map[string]any{"inputs": nil}, raw, nil)
		got := private.OriginalPayload()
		if (got == nil) != (raw == nil) || len(got) != 0 {
			t.Fatal("nil and empty byte identities conflated")
		}
	}
}

type forbiddenMediaValue struct{}

func (forbiddenMediaValue) String() string { panic("must not format input") }
func (forbiddenMediaValue) MarshalJSON() ([]byte, error) {
	panic("must not serialize input")
}

func TestMediaPayloadRefusesUnsupportedOrUnboundedProjection(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	for name, value := range map[string]any{
		"custom": forbiddenMediaValue{},
		"cycle":  cycle,
		"wide":   make([]any, 4097),
	} {
		t.Run(name, func(t *testing.T) {
			raw := []byte(`{"inputs":"synthetic-invalid-capture"}`)
			private := CaptureMedia(MediaGenerate, map[string]any{"inputs": value}, raw, nil)
			if private == nil {
				t.Fatal("failed capture lost private classification")
			}
			if _, err := private.ExecutionPayload(); !errors.Is(err, ErrMediaProjection) {
				t.Fatal("unbounded or unsupported payload became executable")
			}
			if _, err := private.OriginalData(); !errors.Is(err, ErrMediaProjection) {
				t.Fatal("invalid capture projected metadata")
			}
			if !bytes.Equal(private.OriginalPayload(), raw) {
				t.Fatal("invalid capture lost original byte identity")
			}
			assertOpaqueMedia(t, private, "synthetic-invalid-capture")
		})
	}
	var absent *MediaPayload
	if absent.Clone() != nil || absent.OriginalPayload() != nil {
		t.Fatal("nil capture acquired bytes")
	}
	for _, private := range []*MediaPayload{nil, {}} {
		if _, err := private.ExecutionPayload(); !errors.Is(err, ErrMediaProjection) {
			t.Fatal("nil/zero capture became executable")
		}
	}
}

func TestMediaPayloadLegacyClassificationAndDataFallback(t *testing.T) {
	legacy := []byte(`{"prompt":"` + strings.Repeat("x", 100000) + `","nested":{"inputs":"not root"}}`)
	if CaptureMedia(MediaGenerate, nil, legacy, nil) != nil {
		t.Fatal("valid large legacy text changed retention")
	}
	if CaptureMedia("OTHER", map[string]any{"inputs": "ordinary"}, []byte(`{invalid`), nil) != nil {
		t.Fatal("another kind changed retention")
	}
	for _, value := range []any{`{"inpu\u0074s":null}`, []byte(`{"inputs":false}`), forbiddenMediaValue{}} {
		if CaptureMedia(MediaGenerate, nil, nil, map[string]any{"payload": value}) == nil {
			t.Fatal("raw-data fallback escaped private classification")
		}
	}
	if CaptureMedia(MediaGenerate, nil, nil, map[string]any{"payload": nil}) != nil {
		t.Fatal("absent payload changed retention")
	}
	if !IsPrivateMedia(MediaGenerate, PrivateMediaMarker()) {
		t.Fatal("sealed marker lost private classification")
	}
}

func TestMediaPayloadConcurrentIndependentProjections(t *testing.T) {
	private := CaptureMedia(MediaGenerate, map[string]any{"inputs": map[string]any{"token": "synthetic"}}, []byte(`{"inputs":null}`), nil)
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			for range 20 {
				payload, err := private.Clone().ExecutionPayload()
				if err != nil {
					t.Error(err)
					return
				}
				payload["inputs"].(map[string]any)["token"] = "caller-owned"
				private.OriginalPayload()[0] = 'X'
			}
		})
	}
	group.Wait()
}

func FuzzPrivateMediaRawClassification(f *testing.F) {
	for _, seed := range []string{
		``, ` `, `{}`, `null`, `[]`, `"inputs"`, `{"inputs":null}`,
		`{"inpu\u0074s":false}`, `{"inputs":null,"inputs":0}`,
		`{"nested":{"inputs":1},"prompt":"escaped \\\"{},[]"}`,
		`{"abcdefghijklmnopqrstuvwxy":[],"inputs":[]}`, `{"inputs":`,
		" \n{\"nested\":[{\"key\":[[1]]}],\"inputs\":\"synthetic\"}\t",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		want := false
		if len(raw) > 0 {
			var root map[string]json.RawMessage
			if json.Unmarshal(raw, &root) != nil {
				want = true
			} else if root == nil {
				want = true
			} else {
				_, want = root["inputs"]
			}
		}
		if got := privateMediaRaw(raw); got != want {
			t.Fatalf("private classification mismatch: got %v want %v", got, want)
		}
	})
}
