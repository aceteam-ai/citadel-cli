package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsMeteredPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/completions", true},
		{"/v1/embeddings", true},
		{"/v1/models", false},
		{"/health", false},
		{"/", false},
		{"/v1/chat", false},
	}

	for _, tt := range tests {
		got := isMeteredPath(tt.path)
		if got != tt.want {
			t.Errorf("isMeteredPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestExtractConsumerKey(t *testing.T) {
	tests := []struct {
		auth string
		want string
	}{
		{"Bearer sk-12345678abcdefgh", "sk-12345..."},
		{"Bearer short", "short"},
		{"Bearer 12345678", "12345678"},
		{"", "anonymous"},
		{"Basic dXNlcjpwYXNz", "anonymous"},
	}

	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if tt.auth != "" {
			r.Header.Set("Authorization", tt.auth)
		}
		got := extractConsumerKey(r)
		if got != tt.want {
			t.Errorf("auth=%q: got %q, want %q", tt.auth, got, tt.want)
		}
	}
}

func TestDetectStream(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{`{"model":"gpt-4","stream":true}`, true},
		{`{"model":"gpt-4","stream":false}`, false},
		{`{"model":"gpt-4"}`, false},
		{`invalid json`, false},
	}

	for _, tt := range tests {
		got := detectStream([]byte(tt.body))
		if got != tt.want {
			t.Errorf("detectStream(%q) = %v, want %v", tt.body, got, tt.want)
		}
	}
}

func TestExtractUsageFromBody(t *testing.T) {
	body := `{
		"id": "chatcmpl-123",
		"model": "llama-7b",
		"choices": [{"message": {"content": "Hello!"}}],
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 50,
			"total_tokens": 150
		}
	}`

	usage := extractUsageFromBody([]byte(body))
	if usage.PromptTokens != 100 {
		t.Errorf("prompt_tokens = %d, want 100", usage.PromptTokens)
	}
	if usage.CompletionTokens != 50 {
		t.Errorf("completion_tokens = %d, want 50", usage.CompletionTokens)
	}
	if usage.Model != "llama-7b" {
		t.Errorf("model = %q, want llama-7b", usage.Model)
	}
}

func TestExtractUsageFromBody_NoUsage(t *testing.T) {
	body := `{"id":"chatcmpl-123","model":"llama-7b","choices":[]}`
	usage := extractUsageFromBody([]byte(body))
	if usage.PromptTokens != 0 || usage.CompletionTokens != 0 {
		t.Errorf("expected zero usage, got %+v", usage)
	}
}

func TestExtractUsageFromSSELine(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		want  bool
		in    int
		out   int
		model string
	}{
		{
			name: "usage chunk",
			line: `data: {"model":"llama-7b","usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`,
			want: true, in: 100, out: 50, model: "llama-7b",
		},
		{
			name: "content chunk (no usage)",
			line: `data: {"model":"llama-7b","choices":[{"delta":{"content":"Hello"}}]}`,
			want: false,
		},
		{
			name: "done marker",
			line: `data: [DONE]`,
			want: false,
		},
		{
			name: "empty line",
			line: ``,
			want: false,
		},
		{
			name: "non-data line",
			line: `event: message`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage, ok := extractUsageFromSSELine([]byte(tt.line))
			if ok != tt.want {
				t.Errorf("ok = %v, want %v", ok, tt.want)
			}
			if ok {
				if usage.PromptTokens != tt.in {
					t.Errorf("prompt_tokens = %d, want %d", usage.PromptTokens, tt.in)
				}
				if usage.CompletionTokens != tt.out {
					t.Errorf("completion_tokens = %d, want %d", usage.CompletionTokens, tt.out)
				}
				if usage.Model != tt.model {
					t.Errorf("model = %q, want %q", usage.Model, tt.model)
				}
			}
		})
	}
}

func TestMeteringMiddleware_NonStreaming(t *testing.T) {
	dir := t.TempDir()
	ledger := NewLedger(dir)
	tier, _ := TierByName("small")

	// Backend returns an OpenAI-compatible response with usage
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"id":    "chatcmpl-test",
			"model": "llama-7b",
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": "Hello!"}},
			},
			"usage": map[string]int{
				"prompt_tokens":     200,
				"completion_tokens": 100,
				"total_tokens":      300,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	middleware := NewMeteringMiddleware(backend, ledger, nil, tier)
	server := httptest.NewServer(middleware)
	defer server.Close()

	// Non-streaming request
	body := `{"model":"llama-7b","messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Check in-process stats
	totalIn, totalOut, totalCost, reqCount := middleware.InProcessStats()
	if totalIn != 200 {
		t.Errorf("totalIn = %d, want 200", totalIn)
	}
	if totalOut != 100 {
		t.Errorf("totalOut = %d, want 100", totalOut)
	}
	// Small tier: 300 tokens * 1/1K = ceil(0.3) = 1 ACET
	if totalCost != 1 {
		t.Errorf("totalCost = %d, want 1", totalCost)
	}
	if reqCount != 1 {
		t.Errorf("requestCount = %d, want 1", reqCount)
	}

	// Check ledger
	recent, err := ledger.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 {
		t.Fatalf("ledger has %d records, want 1", len(recent))
	}
	if recent[0].Model != "llama-7b" {
		t.Errorf("model = %q, want llama-7b", recent[0].Model)
	}
}

func TestMeteringMiddleware_Streaming(t *testing.T) {
	dir := t.TempDir()
	ledger := NewLedger(dir)
	tier, _ := TierByName("medium")

	// Backend returns SSE chunks with usage in the final chunk
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}

		// Content chunks
		chunks := []string{
			`data: {"model":"llama-70b","choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"model":"llama-70b","choices":[{"delta":{"content":" world"}}]}`,
			// Final chunk with usage (when stream_options.include_usage=true)
			`data: {"model":"llama-70b","usage":{"prompt_tokens":500,"completion_tokens":200,"total_tokens":700}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "%s\n\n", chunk)
			flusher.Flush()
		}
	})

	middleware := NewMeteringMiddleware(backend, ledger, nil, tier)
	server := httptest.NewServer(middleware)
	defer server.Close()

	body := `{"model":"llama-70b","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read the full response
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)

	// Verify content was passed through
	if !strings.Contains(buf.String(), "Hello") {
		t.Error("response should contain streamed content")
	}

	// Check metering captured the usage
	totalIn, totalOut, totalCost, reqCount := middleware.InProcessStats()
	if totalIn != 500 {
		t.Errorf("totalIn = %d, want 500", totalIn)
	}
	if totalOut != 200 {
		t.Errorf("totalOut = %d, want 200", totalOut)
	}
	// Medium tier: 700 tokens * 5/1K = ceil(3.5) = 4 ACET
	if totalCost != 4 {
		t.Errorf("totalCost = %d, want 4", totalCost)
	}
	if reqCount != 1 {
		t.Errorf("requestCount = %d, want 1", reqCount)
	}
}

func TestMeteringMiddleware_NonMeteredPath(t *testing.T) {
	dir := t.TempDir()
	ledger := NewLedger(dir)
	tier, _ := TierByName("small")

	called := false
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	middleware := NewMeteringMiddleware(backend, ledger, nil, tier)
	server := httptest.NewServer(middleware)
	defer server.Close()

	// Request to a non-metered path
	resp, err := http.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !called {
		t.Error("backend should have been called")
	}

	// No transactions should be recorded
	_, _, _, reqCount := middleware.InProcessStats()
	if reqCount != 0 {
		t.Errorf("requestCount = %d, want 0 for non-metered path", reqCount)
	}
}

func TestMeteringMiddleware_WithACETSettlement(t *testing.T) {
	dir := t.TempDir()
	ledger := NewLedger(dir)
	tier, _ := TierByName("small")

	settleCalled := make(chan bool, 1)
	settleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/acet/settle" {
			settleCalled <- true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer settleServer.Close()

	acet := NewACETClient(settleServer.URL, "test")

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"model": "test-model",
			"usage": map[string]int{
				"prompt_tokens":     100,
				"completion_tokens": 50,
				"total_tokens":      150,
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	middleware := NewMeteringMiddleware(backend, ledger, acet, tier)
	server := httptest.NewServer(middleware)
	defer server.Close()

	body := `{"model":"test-model","messages":[{"role":"user","content":"Hi"}]}`
	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Settlement should happen asynchronously
	select {
	case <-settleCalled:
		// success
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for ACET settlement call")
	}
}

func TestInjectStreamUsageOption(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantKey  bool
		wantVal  bool
	}{
		{
			name:    "adds stream_options when absent",
			input:   `{"model":"gpt-4","stream":true}`,
			wantKey: true,
			wantVal: true,
		},
		{
			name:    "preserves existing stream_options and adds include_usage",
			input:   `{"model":"gpt-4","stream":true,"stream_options":{"other":true}}`,
			wantKey: true,
			wantVal: true,
		},
		{
			name:    "preserves include_usage when already true",
			input:   `{"model":"gpt-4","stream":true,"stream_options":{"include_usage":true}}`,
			wantKey: true,
			wantVal: true,
		},
		{
			name:    "overrides include_usage when false",
			input:   `{"model":"gpt-4","stream":true,"stream_options":{"include_usage":false}}`,
			wantKey: true,
			wantVal: true,
		},
		{
			name:  "returns original on invalid JSON",
			input: `not json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := injectStreamUsageOption([]byte(tt.input))

			if !tt.wantKey {
				// Should be unchanged for invalid input
				if string(result) != tt.input {
					t.Errorf("expected unchanged input for invalid JSON")
				}
				return
			}

			var obj map[string]json.RawMessage
			if err := json.Unmarshal(result, &obj); err != nil {
				t.Fatalf("result is not valid JSON: %v", err)
			}

			soRaw, ok := obj["stream_options"]
			if !ok {
				t.Fatal("stream_options key missing from result")
			}

			var so map[string]interface{}
			if err := json.Unmarshal(soRaw, &so); err != nil {
				t.Fatalf("stream_options is not a JSON object: %v", err)
			}

			includeUsage, ok := so["include_usage"]
			if !ok {
				t.Fatal("include_usage key missing from stream_options")
			}
			if includeUsage != true {
				t.Errorf("include_usage = %v, want true", includeUsage)
			}

			// Verify model field is preserved
			if _, ok := obj["model"]; !ok {
				t.Error("model field lost during injection")
			}
		})
	}
}
