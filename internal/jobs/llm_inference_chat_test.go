package jobs

import "testing"

// TestParseChatCompletionResponse covers the OpenAI chat-completions body
// parsing used by the chat path (executeChatCompletionsAt), including the
// thinking-model reasoning_content fallback that keeps Bonsai from returning a
// blank reply when the token budget is spent mid-reasoning.
func TestParseChatCompletionResponse(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantContent    string
		wantFinish     string
		wantCompletion int
	}{
		{
			name:           "normal content wins over reasoning",
			body:           `{"choices":[{"message":{"content":"42","reasoning_content":"6*7=42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":24,"completion_tokens":131,"total_tokens":155}}`,
			wantContent:    "42",
			wantFinish:     "stop",
			wantCompletion: 131,
		},
		{
			name:        "empty content falls back to reasoning (thinking model, length-capped)",
			body:        `{"choices":[{"message":{"content":"","reasoning_content":"still thinking..."},"finish_reason":"length"}]}`,
			wantContent: "still thinking...",
			wantFinish:  "length",
		},
		{
			name:        "no choices yields empty content and default stop",
			body:        `{"choices":[],"usage":{}}`,
			wantContent: "",
			wantFinish:  "stop",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, finish, usage, err := parseChatCompletionResponse([]byte(tc.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if content != tc.wantContent {
				t.Errorf("content = %q, want %q", content, tc.wantContent)
			}
			if finish != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", finish, tc.wantFinish)
			}
			if tc.wantCompletion != 0 {
				if got, _ := usage["completion_tokens"].(int); got != tc.wantCompletion {
					t.Errorf("completion_tokens = %v, want %d", usage["completion_tokens"], tc.wantCompletion)
				}
			}
		})
	}

	if _, _, _, err := parseChatCompletionResponse([]byte(`not json`)); err == nil {
		t.Error("expected error for malformed body, got nil")
	}
}
