package jobs

import (
	"encoding/json"
	"testing"
)

// TestLLMInferencePayload_ResponseFormatAndThinkRoundTrip pins the additive S2
// fields (aceteam-ai/aceteam#9817): response_format carries an arbitrary JSON
// Schema shape verbatim (raw JSON, never re-typed), and think is a tri-state
// *bool whose omitempty omits ONLY the nil case so an explicit false still
// serializes as `"think":false`.
func TestLLMInferencePayload_ResponseFormatAndThinkRoundTrip(t *testing.T) {
	rawIn := []byte(`{
		"model": "qwen3.5:9b",
		"backend": "ollama",
		"messages": [{"role": "user", "content": "extract"}],
		"response_format": {
			"type": "json_schema",
			"json_schema": {
				"name": "resume",
				"schema": {"type": "object", "properties": {"years": {"type": "number"}}},
				"strict": true
			}
		},
		"think": false
	}`)

	var p LLMInferencePayload
	if err := json.Unmarshal(rawIn, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// think:false decodes to a non-nil pointer to false (tri-state present).
	if p.Think == nil {
		t.Fatal("Think = nil, want non-nil pointer to false")
	}
	if *p.Think != false {
		t.Errorf("*Think = %v, want false", *p.Think)
	}

	// response_format is carried raw; its inner schema must be intact.
	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(p.ResponseFormat, &rf); err != nil {
		t.Fatalf("response_format did not carry a parseable object: %v", err)
	}
	if rf.Type != "json_schema" {
		t.Errorf("response_format.type = %q, want json_schema", rf.Type)
	}
	var schema struct {
		Type       string `json:"type"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(rf.JSONSchema.Schema, &schema); err != nil {
		t.Fatalf("inner schema did not survive round-trip: %v", err)
	}
	if schema.Properties["years"].Type != "number" {
		t.Errorf("schema.properties.years.type = %q, want number", schema.Properties["years"].Type)
	}

	// Re-marshal: think:false must be present (not omitted by omitempty).
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal re-marshaled: %v", err)
	}
	if v, present := got["think"]; !present {
		t.Error("re-marshaled payload dropped think:false, want it present (tri-state)")
	} else if string(v) != "false" {
		t.Errorf("re-marshaled think = %s, want false", v)
	}
}

// TestLLMInferencePayload_AbsentS2FieldsOmitted pins the back-compat direction:
// a payload that sets neither field leaves ResponseFormat nil and Think nil, and
// re-marshaling emits neither key (omitempty), so a pre-S2 request is unchanged.
func TestLLMInferencePayload_AbsentS2FieldsOmitted(t *testing.T) {
	var p LLMInferencePayload
	if err := json.Unmarshal([]byte(`{"model":"m","backend":"ollama","prompt":"hi"}`), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.ResponseFormat != nil {
		t.Errorf("ResponseFormat = %s, want nil when absent", p.ResponseFormat)
	}
	if p.Think != nil {
		t.Errorf("Think = %v, want nil when absent", p.Think)
	}

	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal re-marshaled: %v", err)
	}
	if _, present := got["response_format"]; present {
		t.Error("re-marshaled payload has a response_format key, want it omitted when absent")
	}
	if _, present := got["think"]; present {
		t.Error("re-marshaled payload has a think key, want it omitted when absent")
	}
}
