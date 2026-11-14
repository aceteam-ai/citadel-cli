package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/jobs"
)

// runSamplingJob runs one llm_inference job against a fake engine and returns
// the decoded outbound request body and the result.
func runSamplingJob(t *testing.T, backend, path, reply string, extra map[string]any) (map[string]any, *JobResult) {
	t.Helper()
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveReadinessProbe(w, r) {
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(reply))
	}))
	defer ts.Close()

	h := NewLLMInferenceHandler()
	h.baseURLs[backend] = ts.URL
	payload := map[string]any{"model": "m", "backend": backend}
	for k, v := range extra {
		payload[k] = v
	}
	result, err := h.Execute(context.Background(), &Job{ID: "j", Type: JobTypeLLMInference, Payload: payload}, &MockStreamWriter{})
	if err != nil || result == nil || result.Status != JobStatusSuccess {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	return got, result
}

const ollamaChatReply = `{"message":{"role":"assistant","content":"ok"},"done":true}`
const ollamaGenReply = `{"response":"ok","done":true}`
const openAIChatReply = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
const openAICompReply = `{"choices":[{"text":"ok","finish_reason":"stop"}]}`

var chatMsgs = []map[string]any{{"role": "user", "content": "hi"}}

func ollamaOpts(t *testing.T, req map[string]any) map[string]any {
	t.Helper()
	o, _ := req["options"].(map[string]any)
	return o
}

func TestOllamaChatTemperatureZeroAndSeed(t *testing.T) {
	req, res := runSamplingJob(t, "ollama", "", ollamaChatReply, map[string]any{
		"messages": chatMsgs, "temperature": 0, "seed": 42})
	o := ollamaOpts(t, req)
	if v, ok := o["temperature"]; !ok || v.(float64) != 0 {
		t.Errorf("options = %+v, want temperature 0", o)
	}
	if v, ok := o["seed"]; !ok || v.(float64) != 42 {
		t.Errorf("options = %+v, want seed 42", o)
	}
	want := map[string]any{"temperature": float64(0), "seed": int64(42)}
	if !reflect.DeepEqual(res.Output["sampling"], want) {
		t.Errorf("sampling = %#v, want %#v", res.Output["sampling"], want)
	}
}

func TestOllamaGenerateTemperatureZeroAndSeed(t *testing.T) {
	req, res := runSamplingJob(t, "ollama", "", ollamaGenReply, map[string]any{
		"prompt": "hi", "temperature": 0, "seed": 7})
	o := ollamaOpts(t, req)
	if v, ok := o["temperature"]; !ok || v.(float64) != 0 {
		t.Errorf("options = %+v, want temperature 0", o)
	}
	if v, ok := o["seed"]; !ok || v.(float64) != 7 {
		t.Errorf("options = %+v, want seed 7", o)
	}
	if res.Output["sampling"] == nil {
		t.Error("sampling missing from output")
	}
}

func TestOllamaAbsentSamplingNotSent(t *testing.T) {
	for name, tc := range map[string]struct {
		reply string
		extra map[string]any
	}{
		"chat":     {ollamaChatReply, map[string]any{"messages": chatMsgs}},
		"generate": {ollamaGenReply, map[string]any{"prompt": "hi"}},
	} {
		t.Run(name, func(t *testing.T) {
			req, res := runSamplingJob(t, "ollama", "", tc.reply, tc.extra)
			o := ollamaOpts(t, req)
			if _, ok := o["temperature"]; ok {
				t.Errorf("options = %+v, want no temperature", o)
			}
			if _, ok := o["seed"]; ok {
				t.Errorf("options = %+v, want no seed", o)
			}
			if _, ok := res.Output["sampling"]; ok {
				t.Errorf("Output = %+v, want no sampling", res.Output)
			}
		})
	}
}

func TestOpenAIShapedSeedAndTemperature(t *testing.T) {
	// chat completions (vllm + messages)
	req, res := runSamplingJob(t, "vllm", "", openAIChatReply, map[string]any{
		"messages": chatMsgs, "seed": 5, "temperature": 0.3, "top_p": 0.9})
	if req["seed"].(float64) != 5 || req["temperature"].(float64) != 0.3 {
		t.Errorf("chat req = %+v", req)
	}
	want := map[string]any{"temperature": 0.3, "top_p": 0.9, "seed": int64(5)}
	if !reflect.DeepEqual(res.Output["sampling"], want) {
		t.Errorf("sampling = %#v, want %#v", res.Output["sampling"], want)
	}

	// text completions (sglang + prompt)
	req, _ = runSamplingJob(t, "sglang", "", openAICompReply, map[string]any{"prompt": "hi", "seed": 9})
	if req["seed"].(float64) != 9 {
		t.Errorf("completions req = %+v, want seed 9", req)
	}

	// llama.cpp /completion
	req, _ = runSamplingJob(t, "llamacpp", "", `{"content":"ok","stop":true}`, map[string]any{"prompt": "hi", "seed": 11})
	if req["seed"].(float64) != 11 {
		t.Errorf("llamacpp req = %+v, want seed 11", req)
	}
}

func TestOpenAIShapedAbsentKeepsWire(t *testing.T) {
	req, res := runSamplingJob(t, "vllm", "", openAIChatReply, map[string]any{"messages": chatMsgs})
	if _, ok := req["seed"]; ok {
		t.Errorf("req = %+v, want no seed", req)
	}
	if v, ok := req["temperature"]; !ok || v.(float64) != 0 {
		t.Errorf("req = %+v, want temperature 0 when absent", req)
	}
	// Temperature 0 is sent on the wire, so it is recorded.
	want := map[string]any{"temperature": float64(0)}
	if !reflect.DeepEqual(res.Output["sampling"], want) {
		t.Errorf("sampling = %#v, want %#v", res.Output["sampling"], want)
	}
}

func TestRunnerOutputManifestSampling(t *testing.T) {
	without := runnerOutputManifest(map[string]any{"content": "x"})
	b, _ := json.Marshal(without)
	if string(b) != `{"content":"x"}` {
		t.Errorf("manifest without sampling = %s", b)
	}
	with := runnerOutputManifest(map[string]any{"content": "x", "sampling": map[string]any{"seed": int64(1)}})
	b, _ = json.Marshal(with)
	if string(b) != `{"content":"x","sampling":{"seed":1}}` {
		t.Errorf("manifest with sampling = %s", b)
	}
}

func TestPayloadTemperaturePresence(t *testing.T) {
	var a, b jobs.LLMInferencePayload
	if err := json.Unmarshal([]byte(`{"temperature":0}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{}`), &b); err != nil {
		t.Fatal(err)
	}
	if a.Temperature == nil || *a.Temperature != 0 {
		t.Errorf("explicit 0 decoded to %v, want non-nil 0", a.Temperature)
	}
	if b.Temperature != nil {
		t.Errorf("absent decoded to %v, want nil", b.Temperature)
	}
}

func TestParseLLMInferencePayloadRejectsUnsafeSeed(t *testing.T) {
	base := func(seed any) map[string]any {
		return map[string]any{"model": "m", "prompt": "p", "seed": seed}
	}
	for _, bad := range []any{1.5, float64(1 << 54), "7"} {
		if _, err := parseLLMInferencePayload(base(bad)); err == nil {
			t.Fatalf("seed %v accepted", bad)
		}
	}
	p, err := parseLLMInferencePayload(base(float64(42)))
	if err != nil || p.Seed == nil || *p.Seed != 42 {
		t.Fatalf("seed 42: %v %v", p, err)
	}
}

func TestSamplingSentSGLangMessagesOmitsTopP(t *testing.T) {
	p := &jobs.LLMInferencePayload{Backend: "sglang", Messages: []jobs.ChatMessage{{Role: "user"}}, TopP: 0.9}
	if _, ok := samplingSent(p)["top_p"]; ok {
		t.Fatal("sglang never sends top_p, so it must not be recorded")
	}
}
