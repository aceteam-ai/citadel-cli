package localchat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aceteam-ai/citadel-cli/internal/externalengine"
)

func TestNewClientUsesSharedVLLMEndpointAndIsolatesOtherEngines(t *testing.T) {
	original := resolveVLLMEndpoint
	t.Cleanup(func() { resolveVLLMEndpoint = original })

	resolveVLLMEndpoint = func() (externalengine.Endpoint, bool, error) {
		return externalengine.Endpoint{Host: "192.0.2.10", Port: 58000}, true, nil
	}
	client, err := NewClient("vllm", 58000, "vendor/model")
	if err != nil {
		t.Fatalf("NewClient(vllm) error = %v", err)
	}
	if client.BaseURL != "http://192.0.2.10:58000" {
		t.Fatalf("vLLM BaseURL = %q, want resolved override", client.BaseURL)
	}

	resolveVLLMEndpoint = func() (externalengine.Endpoint, bool, error) {
		t.Fatal("non-vLLM client must not resolve the vLLM endpoint")
		return externalengine.Endpoint{}, false, nil
	}
	client, err = NewClient("ollama", 11434, "llama3")
	if err != nil {
		t.Fatalf("NewClient(ollama) error = %v", err)
	}
	if client.BaseURL != "http://localhost:11434" {
		t.Fatalf("ollama BaseURL = %q, want localhost isolation", client.BaseURL)
	}
}

func TestNewClientUsesIPv4LoopbackVLLMEndpoint(t *testing.T) {
	original := resolveVLLMEndpoint
	resolveVLLMEndpoint = func() (externalengine.Endpoint, bool, error) {
		return externalengine.Endpoint{Host: "127.0.0.1", Port: 8201}, true, nil
	}
	t.Cleanup(func() { resolveVLLMEndpoint = original })

	client, err := NewClient("vllm", 8201, "model")
	if err != nil {
		t.Fatalf("NewClient(vllm) error = %v", err)
	}
	if client.BaseURL != "http://127.0.0.1:8201" {
		t.Fatalf("default vLLM BaseURL = %q, want IPv4 loopback URL", client.BaseURL)
	}
}

func TestNewClientFormatsIPv6VLLMHost(t *testing.T) {
	original := resolveVLLMEndpoint
	resolveVLLMEndpoint = func() (externalengine.Endpoint, bool, error) {
		return externalengine.Endpoint{Host: "2001:db8::10", Port: 58000}, true, nil
	}
	t.Cleanup(func() { resolveVLLMEndpoint = original })

	client, err := NewClient("vllm", 58000, "model")
	if err != nil {
		t.Fatalf("NewClient(vllm) error = %v", err)
	}
	if client.BaseURL != "http://[2001:db8::10]:58000" {
		t.Fatalf("IPv6 vLLM BaseURL = %q", client.BaseURL)
	}
}

func TestBuildRequestBody_DefaultsAndFields(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hi"},
	}

	// maxTokens <= 0 falls back to the default.
	body, err := BuildRequestBody("my-model", msgs, 0)
	if err != nil {
		t.Fatalf("BuildRequestBody: %v", err)
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Model != "my-model" {
		t.Errorf("model = %q, want my-model", req.Model)
	}
	if !req.Stream {
		t.Error("stream should be true")
	}
	if req.MaxTokens != DefaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", req.MaxTokens, DefaultMaxTokens)
	}
	if DefaultMaxTokens < 512 {
		t.Errorf("DefaultMaxTokens %d below issue floor of 512", DefaultMaxTokens)
	}
	if len(req.Messages) != 2 || req.Messages[1].Content != "hi" {
		t.Errorf("messages not preserved: %+v", req.Messages)
	}

	// Explicit positive maxTokens is honored.
	body, _ = BuildRequestBody("m", msgs, 42)
	_ = json.Unmarshal(body, &req)
	if req.MaxTokens != 42 {
		t.Errorf("max_tokens = %d, want 42", req.MaxTokens)
	}
}

func TestParseSSELine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantChunk StreamChunk
		handled   bool
		done      bool
		wantErr   bool
	}{
		{name: "blank", line: "", handled: false},
		{name: "comment", line: ": keep-alive", handled: false},
		{name: "non-data", line: "event: message", handled: false},
		{name: "done", line: "data: [DONE]", done: true},
		{
			name:      "reasoning only (content null)",
			line:      `data: {"choices":[{"delta":{"content":null,"reasoning_content":"Here"}}]}`,
			wantChunk: StreamChunk{Reasoning: "Here"},
			handled:   true,
		},
		{
			name:      "answer content",
			line:      `data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			wantChunk: StreamChunk{Content: "Hello"},
			handled:   true,
		},
		{
			name:      "role-only opening delta",
			line:      `data: {"choices":[{"delta":{"role":"assistant","content":null}}]}`,
			wantChunk: StreamChunk{},
			handled:   true,
		},
		{
			name:    "empty choices",
			line:    `data: {"choices":[]}`,
			handled: true,
		},
		{
			name:      "no data prefix space",
			line:      `data:{"choices":[{"delta":{"content":"x"}}]}`,
			wantChunk: StreamChunk{Content: "x"},
			handled:   true,
		},
		{
			name:    "malformed json",
			line:    `data: {not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunk, handled, done, err := ParseSSELine(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if handled != tt.handled {
				t.Errorf("handled = %v, want %v", handled, tt.handled)
			}
			if done != tt.done {
				t.Errorf("done = %v, want %v", done, tt.done)
			}
			if chunk != tt.wantChunk {
				t.Errorf("chunk = %+v, want %+v", chunk, tt.wantChunk)
			}
		})
	}
}

// TestStream_EndToEnd drives Stream against a fake SSE server that emits the
// Bonsai-shaped sequence (reasoning first, then answer, then [DONE]) and asserts
// the chunks arrive in order and split cleanly into reasoning vs answer.
func TestStream_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		lines := []string{
			`data: {"choices":[{"delta":{"role":"assistant","content":null}}]}`,
			`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
			`data: {"choices":[{"delta":{"reasoning_content":"ing"}}]}`,
			`: keep-alive`,
			`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
			`data: {"choices":[{"delta":{"content":"!"}}]}`,
			`data: [DONE]`,
		}
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}

	var reasoning, answer strings.Builder
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, 64, func(ch StreamChunk) {
		reasoning.WriteString(ch.Reasoning)
		answer.WriteString(ch.Content)
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if reasoning.String() != "thinking" {
		t.Errorf("reasoning = %q, want %q", reasoning.String(), "thinking")
	}
	if answer.String() != "Hi!" {
		t.Errorf("answer = %q, want %q", answer.String(), "Hi!")
	}
}

func TestStream_Non200SurfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad model"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}
	err := c.Stream(context.Background(), []Message{{Role: "user", Content: "hi"}}, 64, func(StreamChunk) {})
	if err == nil {
		t.Fatal("expected error on non-200")
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("error should surface body, got: %v", err)
	}
}
