package redis

import (
	"context"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/jobprivacy"
	lua "github.com/yuin/gopher-lua"
)

func mediaClassifierState(t testing.TB) (*lua.LState, *lua.LFunction) {
	t.Helper()
	state := lua.NewState(lua.Options{SkipOpenLibs: true})
	t.Cleanup(state.Close)
	lua.OpenString(state)
	state.Pop(1)
	// No OS, IO, loading, network, cjson or Redis APIs are registered.
	for _, name := range []string{"os", "io", "redis", "cjson", "require", "load", "dofile"} {
		if state.GetGlobal(name) != lua.LNil {
			t.Fatal("classifier test exposed an unrelated library")
		}
	}
	chunk, err := state.LoadString(mediaACKClassifier)
	if err != nil {
		t.Fatal(err)
	}
	state.Push(chunk)
	if err := state.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	function, ok := state.Get(-1).(*lua.LFunction)
	if !ok {
		t.Fatal("classifier source did not return a pure predicate")
	}
	state.Pop(1)
	return state, function
}

func classifyMediaLua(t testing.TB, state *lua.LState, function *lua.LFunction, kind, raw string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state.SetContext(ctx)
	if err := state.CallByParam(lua.P{Fn: function, NRet: 1, Protect: true}, lua.LString(kind), lua.LString(raw)); err != nil {
		t.Fatal(err)
	}
	result, ok := state.Get(-1).(lua.LBool)
	if !ok {
		t.Fatal("classifier returned execution data instead of a boolean")
	}
	state.Pop(1)
	if state.GetTop() != 0 {
		t.Fatal("classifier left input data on its Lua stack")
	}
	return bool(result)
}

func TestMediaACKClassifierParity(t *testing.T) {
	state, function := mediaClassifierState(t)
	fixtures := []string{
		``, ` `, `{}`, `null`, `[]`, `"inputs"`, `{"inputs":null}`,
		`{"inpu\u0074s":false}`, `{"\u0069\u006e\u0070\u0075\u0074\u0073":0}`,
		`{"inputs":null,"inputs":0}`, `{"inputs":`, `{"inputs"}`,
		`{"nested":{"inputs":"not-root"},"prompt":"escaped \\\"{},[]"}`,
		`{"legacy":[true,false,null,{},[],0,-1,0.1,1E+30,1e9999]}`,
		`{"legacy":NaN}`, `{"legacy":Infinity}`, `{"legacy":0x10}`, `{"legacy":01}`,
		`{"legacy":1.}`, `{"legacy":+1}`, `{"legacy":1e}`, `{"legacy":1e+}`,
		`{"legacy":[1,]}`, `{"legacy":true,}`, `{"legacy":true} trailing`,
		`{"legacy":"\ud800"}`, `{"legacy":"\udc00"}`, `{"legacy":"\uZZZZ"}`,
		`{"legacy":"\q"}`, `{"legacy":"\"\\\/\b\f\n\r\t"}`,
		`{"long-root-name-greater-than-thirty-eight-characters":[],"inputs":[]}`,
		"{\"legacy\":\"" + string([]byte{0xff, 0x7f}) + "\"}",
		"{\"legacy\":\"control\x00\"}", "{\"legacy\":\"control\x01\"}",
		`{"prompt":"` + strings.Repeat("large-legacy-text", 100000) + `"}`,
		`{"legacy":` + strings.Repeat("[", 9999) + `0` + strings.Repeat("]", 9999) + `}`,
		`{"legacy":` + strings.Repeat("[", 10000) + `0` + strings.Repeat("]", 10000) + `}`,
	}
	for index, raw := range fixtures {
		want := jobprivacy.CaptureMedia(jobprivacy.MediaGenerate, nil, []byte(raw), nil) != nil
		if got := classifyMediaLua(t, state, function, jobprivacy.MediaGenerate, raw); got != want {
			t.Fatalf("fixture %d: Go/Lua retention mismatch: got %v want %v", index, got, want)
		}
		if classifyMediaLua(t, state, function, "OTHER", raw) {
			t.Fatalf("fixture %d: unrelated job retention changed", index)
		}
	}
}

func TestMediaACKClassifierGeneratedJSON(t *testing.T) {
	state, function := mediaClassifierState(t)
	random := rand.New(rand.NewSource(1314))
	var value func(int) any
	value = func(depth int) any {
		kind := random.Intn(7)
		if depth > 4 {
			kind %= 5
		}
		switch kind {
		case 0:
			return nil
		case 1:
			return random.Intn(2) == 0
		case 2:
			return random.Int63() - random.Int63()
		case 3:
			return []json.Number{"1e9999", "-0.125E+7", "0", "1e-9999"}[random.Intn(4)]
		case 4:
			return "escaped \"\\\n\x00 and Unicode \u00e9 \U0001f642"
		case 5:
			items := make([]any, random.Intn(4))
			for i := range items {
				items[i] = value(depth + 1)
			}
			return items
		default:
			return map[string]any{"nested": value(depth + 1), "inputs": value(depth + 1)}
		}
	}
	for index := range 256 {
		encoded, err := json.Marshal(map[string]any{"legacy": value(0), "prompt": value(0)})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{
			string(encoded),
			`{"\u0069nputs":null,` + string(encoded[1:]),
			string(encoded[:len(encoded)-1]),
			string(encoded) + "x",
		} {
			want := jobprivacy.CaptureMedia(jobprivacy.MediaGenerate, nil, []byte(raw), nil) != nil
			if got := classifyMediaLua(t, state, function, jobprivacy.MediaGenerate, raw); got != want {
				t.Fatalf("generated fixture %d: Go/Lua mismatch: got %v want %v", index, got, want)
			}
		}
	}
}

func FuzzMediaACKClassifierParity(f *testing.F) {
	for _, raw := range []string{``, `{}`, `{"inputs":null}`, `{"inpu\u0074s":false}`, `{"prompt":"x"}`, `{"nested":[{},[]]}`, `{"legacy":NaN}`, `{"legacy":"\ud800"}`, `{"inputs":`} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 65536 {
			t.Skip("bounded local fuzzer work, not a production payload-size rule")
		}
		state, function := mediaClassifierState(t)
		want := jobprivacy.CaptureMedia(jobprivacy.MediaGenerate, nil, raw, nil) != nil
		if got := classifyMediaLua(t, state, function, jobprivacy.MediaGenerate, string(raw)); got != want {
			t.Fatalf("Go/Lua retention mismatch: got %v want %v", got, want)
		}
	})
}
